package http

import (
	"encoding/json"
	"net/http"
	"strconv"
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
	Question  string          `json:"question"`  // optional: user's follow-up / revision request for this stage
	Current   string          `json:"current"`   // optional: current output of this stage, to be revised
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

	// Optional revision: the user asked a follow-up / requested changes on this
	// stage's existing output. Feed both back and ask for a revised version that
	// keeps the same output format the stage's task requires.
	question := strings.TrimSpace(req.Question)
	userMsg := "Kerjakan tahap \"" + stage.title + "\" sekarang."
	if question != "" {
		if len(question) > 2000 {
			question = question[:2000]
		}
		current := strings.TrimSpace(req.Current)
		if current != "" {
			if len(current) > 6000 {
				current = current[:6000] + " …(dipotong)"
			}
			b.WriteString("\nOUTPUT TAHAP INI SAAT INI (untuk direvisi):\n" + current + "\n")
		}
		b.WriteString("\nPERMINTAAN/PERTANYAAN PENGGUNA untuk tahap ini: " + question + "\n")
		b.WriteString("Revisi & lengkapi output tahap ini sesuai permintaan pengguna di atas. " +
			"Pertahankan FORMAT keluaran yang sama persis seperti yang diminta tugas tahap ini " +
			"(mis. tetap JSON murni bila tahap ini berformat JSON).\n")
		userMsg = "Revisi tahap \"" + stage.title + "\" sesuai permintaan pengguna."
	}

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: userMsg},
	}
	out, err := h.ai.Chat(r.Context(), msgs)
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, orchestrateResponse{Stage: req.Stage, Output: out})
}

// ---- Meta Ads: multi-agent PKPSICOV generator ----

// metaAgentRequest runs ONE expert agent of the Meta Ads pipeline. The frontend
// calls it once per agent in order, threading each agent's output into `prior`
// for the next, and renders the final `synthesis` agent's JSON as a dashboard.
type metaAgentRequest struct {
	Agent string          `json:"agent"` // known key (synthesis) OR an ad-hoc/dynamic id
	Model string          `json:"model"` // optional: per-run model override (dynamic picker)
	Ads   json.RawMessage `json:"ads"`   // compact Meta Ads snapshot (totals + campaigns + signals)
	Prior json.RawMessage `json:"prior"` // outputs of the already-completed agents (map)

	// Optional inline PKPSICOV frame for a DYNAMIC agent decided by the planner.
	// When Agent doesn't match a built-in, these describe the expert to run.
	Title      string `json:"title"`
	Peranan    string `json:"peranan"`
	Kompetensi string `json:"kompetensi"`
	Instruksi  string `json:"instruksi"`
	Output     string `json:"output"`
}

type metaAgentResponse struct {
	Agent  string `json:"agent"`
	Output string `json:"output"`
}

// metaAgentFrame is the PKPSICOV frame an agent runs with (built-in or dynamic).
type metaAgentFrame struct{ title, peranan, kompetensi, instruksi, output string }

// metaAgents is the fixed expert panel. Each runs the PKPSICOV frame (Peranan,
// Kompetensi, Pengalaman, Skenario, Instruksi, Constraints, Output, Validation)
// on the same live Meta Ads data. The final `synthesis` agent merges them into a
// structured executive dashboard (JSON) that replaces the Meta Ads view.
var metaAgents = map[string]metaAgentFrame{
	"mediabuyer": {
		title:      "Media Buyer / Performance",
		peranan:    "Media Buyer senior Meta Ads untuk developer properti residensial, pengalaman 15 tahun.",
		kompetensi: "Efisiensi funnel iklan: Spend → Impressions → Reach → Clicks (CTR) → Hasil (chat WA/lead); metrik CPR, CPC, CPM, conversion rate.",
		instruksi:  "Analisis performa: campaign pemenang vs boros, CPR di atas ambang, spend nihil-hasil, dan efisiensi funnel. Sebut nama campaign & angka nyata.",
		output:     "3-5 temuan ringkas berpoin, tiap poin ada angka pendukung. Maksimal 180 kata.",
	},
	"creative": {
		title:      "Creative & Copywriter",
		peranan:    "Creative strategist & copywriter iklan properti, pengalaman 12 tahun.",
		kompetensi: "Daya tarik materi/copy: CTR lemah, ad fatigue (frequency tinggi), angle pesan, hook, dan call-to-action untuk pasar properti Indonesia.",
		instruksi:  "Nilai kualitas kreatif dari sinyal data (CTR rendah, frequency tinggi = fatigue). Usulkan 2-3 angle/hook copy baru yang konkret untuk campaign terlemah.",
		output:     "Poin temuan + daftar usulan angle kreatif konkret. Maksimal 180 kata.",
	},
	"budget": {
		title:      "Budget & Audience Strategist",
		peranan:    "Ahli alokasi budget & targeting Meta Ads, pengalaman 14 tahun.",
		kompetensi: "Realokasi budget ke pemenang, struktur Campaign→Ad Set→Ad, targeting broad vs behavior/exclusion, keluar dari fase learning.",
		instruksi:  "Rekomendasikan realokasi budget spesifik (naikkan/pangkas campaign mana), perbaikan targeting, dan konsolidasi struktur. Sebut angka & nama campaign.",
		output:     "Rekomendasi realokasi berpoin dengan arah angka (mis. +30%/pekan, matikan). Maksimal 180 kata.",
	},
	"synthesis": {
		title:      "Head of Marketing — Sintesis",
		peranan:    "Head of Marketing yang memimpin panel ahli di atas dan memutuskan untuk direksi.",
		kompetensi: "Menggabungkan analisis performa, kreatif, dan budget jadi satu dashboard eksekutif yang actionable.",
		instruksi: `Sintesis SEMUA hasil ahli + data jadi DASHBOARD EKSEKUTIF Meta Ads.
Balas HANYA JSON valid (tanpa markdown/code fence), bentuk:
{
  "title": "judul ringkas",
  "kpis": [ {"label":"...", "value":"...", "note":"konteks singkat", "tone":"ok|warn|bad|neutral"} ],
  "sections": [ {"heading":"...", "items":[ {"title":"...", "detail":"1 kalimat", "tone":"ok|warn|bad|neutral"} ]} ]
}
Aturan: 4-8 KPI dari angka NYATA (format Rupiah bila uang); sections wajib mencakup "Sorotan", "Temuan per Ahli", "Rekomendasi Prioritas"; ringkas & actionable; JANGAN mengarang angka.`,
		output: "HANYA JSON dashboard sesuai format di atas.",
	},
}

// aiMetaAgent runs one expert agent of the Meta Ads PKPSICOV pipeline, grounded
// on the live Meta Ads snapshot + prior agents' outputs. Gated by requireAuth.
func (h *Handler) aiMetaAgent(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"AI belum dikonfigurasi. Set OLLAMA_API_KEY pada service auth (:8090).")
		return
	}
	var req metaAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	// Resolve the frame: a built-in agent (e.g. the fixed "synthesis" finalizer)
	// takes precedence; otherwise use the inline PKPSICOV frame supplied by the
	// planner for a dynamic agent.
	ag, ok := metaAgents[strings.ToLower(strings.TrimSpace(req.Agent))]
	if !ok {
		if strings.TrimSpace(req.Instruksi) == "" && strings.TrimSpace(req.Peranan) == "" {
			writeError(w, http.StatusBadRequest, "agent tidak dikenal & tidak ada spec dinamis: "+req.Agent)
			return
		}
		ag = metaAgentFrame{
			title:      firstNonEmpty(req.Title, req.Agent, "Ahli"),
			peranan:    req.Peranan,
			kompetensi: req.Kompetensi,
			instruksi:  req.Instruksi,
			output:     firstNonEmpty(req.Output, "3-5 temuan ringkas berpoin dengan angka pendukung. Maksimal 180 kata."),
		}
	}

	var b strings.Builder
	b.WriteString("Kamu adalah salah satu AHLI dalam panel AI Marketing Greenpark yang menganalisis IKLAN META ADS. ")
	b.WriteString("Bahasa Indonesia, ringkas, profesional, berbasis angka — JANGAN mengarang data.\n\n")
	b.WriteString("Gunakan kerangka PKPSICOV:\n")
	b.WriteString("- PERANAN: " + ag.peranan + "\n")
	b.WriteString("- KOMPETENSI: " + ag.kompetensi + "\n")
	b.WriteString("- SKENARIO: Kadep Marketing Greenpark mengevaluasi iklan Meta (properti, Rupiah). Data insight dilampirkan.\n")
	b.WriteString("- INSTRUKSI: " + ag.instruksi + "\n")
	b.WriteString("- CONSTRAINTS: Hanya angka dari data terlampir. Rupiah (IDR), konteks properti Indonesia. Tandai bila data kosong.\n")
	b.WriteString("- OUTPUT: " + ag.output + "\n")
	b.WriteString("- VALIDATION: Tandai temuan yang perlu dicek Kadep; \"hasil\" iklan = chat WA (bukan penjualan), jadi ROAS tak dapat dihitung.\n")

	ads := strings.TrimSpace(string(req.Ads))
	if ads != "" && ads != "null" {
		if len(ads) > maxContextChars {
			ads = ads[:maxContextChars] + " …(dipotong)"
		}
		b.WriteString("\nDATA IKLAN META ADS (JSON, sumber kebenaran):\n" + ads + "\n")
	}
	prior := strings.TrimSpace(string(req.Prior))
	if prior != "" && prior != "null" && prior != "{}" {
		if len(prior) > 6000 {
			prior = prior[:6000] + " …(dipotong)"
		}
		b.WriteString("\nHASIL AHLI SEBELUMNYA (pertimbangkan & bangun di atasnya):\n" + prior + "\n")
	}

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: "Kerjakan peran \"" + ag.title + "\" sekarang."},
	}
	out, err := h.ai.ChatModel(r.Context(), msgs, strings.TrimSpace(req.Model))
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, metaAgentResponse{Agent: req.Agent, Output: out})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// metaPlanRequest asks the AI to DESIGN the expert panel for this dataset — it
// decides how many analysts are needed (not a fixed four) and their PKPSICOV
// frames. The synthesis finalizer is appended separately by the caller.
type metaPlanRequest struct {
	Model string          `json:"model"`
	Ads   json.RawMessage `json:"ads"`
}

type plannedAgent struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Icon       string `json:"icon"`
	Peranan    string `json:"peranan"`
	Kompetensi string `json:"kompetensi"`
	Instruksi  string `json:"instruksi"`
	Output     string `json:"output"`
}

type metaPlanResponse struct {
	Agents   []plannedAgent `json:"agents"`
	Fallback bool           `json:"fallback"` // true when the AI plan couldn't be parsed
}

// defaultPlan is the static panel used as a resilient fallback when the planner
// output can't be parsed, so the pipeline never breaks.
func defaultPlan() []plannedAgent {
	icons := map[string]string{"mediabuyer": "📈", "creative": "✍", "budget": "🎯"}
	out := make([]plannedAgent, 0, 3)
	for _, k := range []string{"mediabuyer", "creative", "budget"} {
		a := metaAgents[k]
		out = append(out, plannedAgent{
			Key: k, Title: a.title, Icon: icons[k],
			Peranan: a.peranan, Kompetensi: a.kompetensi, Instruksi: a.instruksi, Output: a.output,
		})
	}
	return out
}

// aiMetaPlan lets the AI decide the expert panel dynamically for the given Meta
// Ads snapshot. Returns 2–5 analyst agents (never the synthesis finalizer).
func (h *Handler) aiMetaPlan(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"AI belum dikonfigurasi. Set OLLAMA_API_KEY pada service auth (:8090).")
		return
	}
	var req metaPlanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}

	var b strings.Builder
	b.WriteString("Kamu adalah Head of Marketing Greenpark yang MEMIMPIN panel AI untuk menganalisis IKLAN META ADS (properti, Rupiah).\n")
	b.WriteString("TUGASMU: merancang panel ahli yang PALING RELEVAN untuk data iklan di bawah. ")
	b.WriteString("KAMU yang menentukan BERAPA banyak ahli (antara 2 sampai 5) dan bidang apa saja — sesuaikan dengan kondisi data, JANGAN paksakan jumlah tetap.\n")
	b.WriteString("Contoh bidang yang MUNGKIN relevan (pilih/adaptasi/atau buat lain): media buying & performa funnel, kreatif & copywriting, budget & audience, konversi & funnel, skalabilitas campaign pemenang, pencegahan pemborosan, kualitas lead/chat WA.\n")
	b.WriteString("Untuk tiap ahli tulis kerangka PKPSICOV.\n\n")
	b.WriteString("Balas HANYA JSON valid (tanpa markdown/code fence), bentuk PERSIS:\n")
	b.WriteString(`{"agents":[{"key":"slug-unik","title":"Nama Peran","icon":"satu emoji","peranan":"...","kompetensi":"...","instruksi":"analisis spesifik, wajib menyebut angka nyata","output":"format ringkas, maks 180 kata"}]}` + "\n")
	b.WriteString("Aturan: 2-5 ahli, HANYA yang benar-benar relevan dengan data ini; instruksi berbasis angka nyata; Bahasa Indonesia; JANGAN sertakan agent sintesis/kesimpulan akhir (ditangani terpisah).\n")

	ads := strings.TrimSpace(string(req.Ads))
	if ads != "" && ads != "null" {
		if len(ads) > maxContextChars {
			ads = ads[:maxContextChars] + " …(dipotong)"
		}
		b.WriteString("\nDATA IKLAN META ADS (JSON, sumber kebenaran):\n" + ads + "\n")
	}

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: "Rancang panel ahli untuk data ini sekarang. Balas hanya JSON."},
	}
	out, err := h.ai.ChatModel(r.Context(), msgs, strings.TrimSpace(req.Model))
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
		return
	}

	agents := parsePlan(out)
	if len(agents) == 0 {
		writeJSON(w, http.StatusOK, metaPlanResponse{Agents: defaultPlan(), Fallback: true})
		return
	}
	writeJSON(w, http.StatusOK, metaPlanResponse{Agents: agents})
}

// parsePlan extracts the {"agents":[...]} object from model output (tolerating
// prose/fences), validates it, and caps the panel at 2–5 agents. Returns nil on
// failure so the caller can fall back to the static panel.
func parsePlan(out string) []plannedAgent {
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		return nil
	}
	var parsed metaPlanResponse
	if err := json.Unmarshal([]byte(out[i:j+1]), &parsed); err != nil {
		return nil
	}
	seen := map[string]bool{}
	cleaned := make([]plannedAgent, 0, len(parsed.Agents))
	for idx, a := range parsed.Agents {
		if strings.TrimSpace(a.Instruksi) == "" && strings.TrimSpace(a.Peranan) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(a.Key))
		if key == "" || seen[key] || key == "synthesis" {
			key = "ahli" + strconv.Itoa(idx+1)
		}
		seen[key] = true
		a.Key = key
		a.Title = firstNonEmpty(a.Title, "Ahli "+strconv.Itoa(idx+1))
		a.Icon = firstNonEmpty(a.Icon, "🔎")
		a.Output = firstNonEmpty(a.Output, "3-5 temuan ringkas berpoin dengan angka. Maksimal 180 kata.")
		cleaned = append(cleaned, a)
		if len(cleaned) >= 5 {
			break
		}
	}
	if len(cleaned) < 2 {
		return nil
	}
	return cleaned
}

// aiModels lists the model ids available to the configured Ollama key so the UI
// can offer a dynamic model picker. Gated by requireAuth.
func (h *Handler) aiModels(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"AI belum dikonfigurasi. Set OLLAMA_API_KEY pada service auth (:8090).")
		return
	}
	models, err := h.ai.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "gagal ambil daftar model: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Models  []string `json:"models"`
		Current string   `json:"current"`
	}{Models: models, Current: h.ai.Model()})
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
