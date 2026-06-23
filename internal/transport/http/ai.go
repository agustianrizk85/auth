package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"greenpark/auth/internal/ai"
)

// maxContextChars caps the grounding payload embedded in the prompt so a heavy
// page (e.g. the sales war-room with thousands of rows) cannot blow the token
// budget. The frontend already sends a trimmed summary; this is a backstop.
const maxContextChars = 14000

// aiChatRequest is the body of POST /api/ai/chat. `context` is the live data of
// the page the user is viewing (any JSON shape), used to ground the answer.
type aiChatRequest struct {
	Messages []ai.Message    `json:"messages"`
	Division string          `json:"division"`
	Page     string          `json:"page"`
	Context  json.RawMessage `json:"context"`
}

type aiChatResponse struct {
	Reply string `json:"reply"`
}

// aiConfigStatus reports whether the assistant has a usable key (never returns
// the key itself).
type aiConfigStatus struct {
	Configured bool   `json:"configured"`
	Model      string `json:"model"`
}

// aiConfigGet returns the current AI configuration status (for the UI to decide
// whether to prompt for a key).
func (h *Handler) aiConfigGet(w http.ResponseWriter, _ *http.Request) {
	if h.ai == nil {
		writeJSON(w, http.StatusOK, aiConfigStatus{Configured: false})
		return
	}
	writeJSON(w, http.StatusOK, aiConfigStatus{Configured: h.ai.Configured(), Model: h.ai.Model()})
}

// aiConfigSet updates the OpenRouter API key (and optionally model) at runtime,
// set from the dashboard UI. Persisted by the client when a key-file is set.
func (h *Handler) aiConfigSet(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil {
		writeError(w, http.StatusServiceUnavailable, "AI client tidak aktif")
		return
	}
	var req struct {
		Key   string `json:"key"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, "API key kosong")
		return
	}
	h.ai.SetKey(req.Key, req.Model)
	writeJSON(w, http.StatusOK, aiConfigStatus{Configured: h.ai.Configured(), Model: h.ai.Model()})
}

// aiChat answers a dashboard question grounded on the current page's data.
// Gated by requireAuth so only logged-in dashboard users can spend the key.
func (h *Handler) aiChat(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"Asisten AI belum dikonfigurasi. Set OPENROUTER_API_KEY pada service auth (:8090).")
		return
	}

	var req aiChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages kosong")
		return
	}

	// Keep only the recent turns to bound tokens; drop any client-sent system role.
	turns := make([]ai.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "user" || role == "assistant" {
			turns = append(turns, ai.Message{Role: role, Content: m.Content})
		}
	}
	if len(turns) > 12 {
		turns = turns[len(turns)-12:]
	}

	msgs := append([]ai.Message{{Role: "system", Content: buildSystemPrompt(req)}}, turns...)

	reply, err := h.ai.Chat(r.Context(), msgs)
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal menjawab: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, aiChatResponse{Reply: reply})
}

// ---- Orchestrator: a sequential 5-stage AI pipeline over all divisions ----

// orchestrateRequest runs ONE stage of the pipeline. The frontend calls it five
// times in order, threading each stage's output into `prior` for the next.
type orchestrateRequest struct {
	Stage     string          `json:"stage"`     // ceo | solusi | permasalahan | overview | approval
	Divisions json.RawMessage `json:"divisions"` // compact per-division data snapshot
	Prior     json.RawMessage `json:"prior"`     // outputs of the already-completed stages
}

type orchestrateResponse struct {
	Stage  string `json:"stage"`
	Output string `json:"output"`
}

// orchestrateStages is the fixed pipeline order (as specified by the product
// owner): CEO sets direction → Solusi proposes → Permasalahan diagnoses →
// Overview synthesizes → Approval lists decisions to sign off.
var orchestrateStages = map[string]struct{ title, task string }{
	"ceo": {
		"CEO — Arah Strategis",
		`Berperan sebagai CEO Greenpark Group. Dari data SEMUA divisi, tetapkan ARAH & PRIORITAS strategis lintas divisi: 3-5 prioritas utama, target kunci, dan fokus eksekusi minggu ini. Tegas, berbasis angka.`,
	},
	"solusi": {
		"Solusi",
		`Berperan sebagai tim strategi. Berdasarkan arah CEO dan data divisi, usulkan SOLUSI konkret & actionable untuk mencapai prioritas tersebut. Untuk tiap solusi sebutkan divisi terkait, langkah, dan dampak yang diharapkan.`,
	},
	"permasalahan": {
		"Permasalahan",
		`Berperan sebagai auditor internal. Identifikasi PERMASALAHAN, risiko, dan hambatan utama lintas divisi yang menghalangi arah CEO & solusi di atas. Sebut angka nyata, akar masalah, dan divisi yang terdampak. Urut dari paling kritis.`,
	},
	"overview": {
		"Dashboard Eksekutif",
		`Sintesis semua tahap menjadi DASHBOARD EKSEKUTIF terstruktur untuk direksi.
Balas HANYA JSON valid (tanpa markdown/code fence), bentuk:
{
  "title": "judul ringkas",
  "kpis": [ {"label":"...", "value":"...", "note":"konteks singkat", "tone":"ok|warn|bad|neutral"} ],
  "sections": [ {"heading":"...", "items":[ {"title":"...", "detail":"1 kalimat", "tone":"ok|warn|bad|neutral"} ]} ]
}
Aturan: 4-8 KPI lintas divisi (angka nyata dari data, format Rupiah bila uang); 2-4 sections (mis. "Sorotan", "Risiko Utama", "Prioritas Eksekusi"); ringkas & actionable; jangan mengarang angka.`,
	},
	"approval": {
		"Keputusan untuk Persetujuan",
		`Susun DAFTAR KEPUTUSAN/AKSI yang perlu disetujui direktur, hasil sintesis tahap-tahap di atas.
Balas HANYA JSON array valid (tanpa markdown), maksimal 6 item, tiap item:
{"judul": "...", "divisi": "...", "pic": "...", "dampak": "...", "rekomendasi": "setujui" | "tinjau" | "tolak"}`,
	},
}

// aiOrchestrate runs one pipeline stage grounded on all-division data + prior
// stage outputs. Gated by requireAuth (intended for CEO/Dirops).
func (h *Handler) aiOrchestrate(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"Asisten AI belum dikonfigurasi. Set OPENROUTER_API_KEY pada service auth (:8090).")
		return
	}
	var req orchestrateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	stage, ok := orchestrateStages[strings.ToLower(strings.TrimSpace(req.Stage))]
	if !ok {
		writeError(w, http.StatusBadRequest, "stage tidak dikenal: "+req.Stage)
		return
	}

	var b strings.Builder
	b.WriteString("Kamu adalah ORCHESTRATOR AI Greenpark Group yang menganalisis seluruh divisi ")
	b.WriteString("(Perencanaan, Legal/Permit, Marketing, Sales, Keuangan) untuk pengambilan keputusan direksi. ")
	b.WriteString("Bahasa Indonesia, ringkas, profesional, berbasis angka — JANGAN mengarang data.\n\n")
	b.WriteString("TAHAP SAAT INI: " + stage.title + "\n")
	b.WriteString("TUGAS: " + stage.task + "\n")

	divisions := strings.TrimSpace(string(req.Divisions))
	if divisions != "" && divisions != "null" {
		if len(divisions) > maxContextChars {
			divisions = divisions[:maxContextChars] + " …(dipotong)"
		}
		b.WriteString("\nDATA SEMUA DIVISI (JSON, sumber kebenaran):\n" + divisions + "\n")
	}
	prior := strings.TrimSpace(string(req.Prior))
	if prior != "" && prior != "null" && prior != "{}" {
		if len(prior) > 6000 {
			prior = prior[:6000] + " …(dipotong)"
		}
		b.WriteString("\nHASIL TAHAP SEBELUMNYA (lanjutkan & bangun di atasnya):\n" + prior + "\n")
	}

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: "Kerjakan tahap \"" + stage.title + "\" sekarang."},
	}
	out, err := h.ai.Chat(r.Context(), msgs)
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, orchestrateResponse{Stage: req.Stage, Output: out})
}

// buildSystemPrompt grounds the assistant on the Greenpark dashboard and the
// specific page + live data the user is currently viewing.
func buildSystemPrompt(req aiChatRequest) string {
	var b strings.Builder
	b.WriteString(`Kamu adalah "Asisten Dashboard Greenpark Group" — asisten AI yang membantu pengguna `)
	b.WriteString(`memahami dashboard internal (divisi Perencanaan, Legal/Permit, Marketing, Sales, Keuangan). `)
	b.WriteString("Jawab dalam Bahasa Indonesia yang ringkas, jelas, dan profesional. ")
	b.WriteString("Gunakan format Rupiah untuk uang. Jika data tidak cukup untuk menjawab, katakan dengan jujur — JANGAN mengarang angka. ")
	b.WriteString("Boleh bantu menjelaskan istilah, menavigasi fitur, dan menganalisis angka yang tersedia.\n\n")

	division := strings.TrimSpace(req.Division)
	if division == "" {
		division = "(tidak diketahui)"
	}
	page := strings.TrimSpace(req.Page)
	b.WriteString("KONTEKS HALAMAN SAAT INI:\n")
	b.WriteString("- Divisi: " + division + "\n")
	if page != "" {
		b.WriteString("- Halaman/Tab: " + page + "\n")
	}

	ctx := strings.TrimSpace(string(req.Context))
	if ctx != "" && ctx != "null" {
		if len(ctx) > maxContextChars {
			ctx = ctx[:maxContextChars] + " …(data dipotong)"
		}
		b.WriteString("\nDATA HALAMAN (JSON, sumber kebenaran untuk pertanyaan tentang angka):\n")
		b.WriteString(ctx)
		b.WriteString("\n\nJawab pertanyaan pengguna berdasarkan data di atas bila relevan.")
	} else {
		b.WriteString("\n(Tidak ada data terstruktur dari halaman ini — jawab secara umum / bantu navigasi.)")
	}
	return b.String()
}
