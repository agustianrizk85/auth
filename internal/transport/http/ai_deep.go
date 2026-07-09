package http

// Deep Analysis — a marketing research pipeline that goes beyond the PKPSICOV
// panel: the planner designs up to 9 RESEARCH agents (plus the synthesis
// finalizer = max 10 agents total), and each agent runs a bounded TOOL LOOP on
// the server: it may search the web (DuckDuckGo, keyless) and open credible
// pages before writing its analysis. Every agent is grounded on the skill
// markdown files in dashboard/skillmd (deep-analysis methodology + credible
// source guidance), the live Meta Ads snapshot, and prior agents' outputs.
//
// Endpoints (all requireAuth):
//   POST /api/ai/deep-plan   → design the research panel (3–9 agents)
//   POST /api/ai/deep-agent  → run ONE agent (tool loop) or the synthesis
//   GET  /api/ai/deep-skills → list the loaded skill files (for the UI)

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"greenpark/auth/internal/ai"
)

// maxDeepAgents caps the research panel; with the synthesis finalizer appended
// by the frontend the pipeline never exceeds 20 agents total.
const maxDeepAgents = 19

// maxToolSteps bounds how many search/open actions one agent may take in its
// first research round.
const maxToolSteps = 8

// maxRetryRounds: when an agent with an external-research mandate tries to
// finish WITHOUT having opened a single source, the server refuses the final
// and sends it back to research with a fresh query strategy — up to this many
// times, each adding retryExtraSteps to the tool budget.
const maxRetryRounds = 2

// retryExtraSteps is the extra tool budget granted per refused final.
const retryExtraSteps = 5

// maxSkillChars caps the concatenated skill text embedded in every prompt.
const maxSkillChars = 9000

// ---- Skill loading (dashboard/skillmd) ----

// deepSkillDirs returns candidate locations of the skill markdown directory:
// the explicit AI_SKILL_DIR env wins; otherwise try paths relative to the
// service working directory (auth/ in dev, deploy root in prod).
func deepSkillDirs() []string {
	if d := strings.TrimSpace(os.Getenv("AI_SKILL_DIR")); d != "" {
		return []string{d}
	}
	return []string{
		filepath.FromSlash("../dashboard/skillmd"),
		filepath.FromSlash("dashboard/skillmd"),
		filepath.FromSlash("skillmd"),
	}
}

// fallbackSkill keeps the pipeline principled even when no skill file is found.
const fallbackSkill = `# SKILL: Deep Analysis (ringkas)
Rumuskan hipotesis dari data lalu cari bukti; pisahkan FAKTA vs INFERENSI vs ASUMSI (tandai asumsi).
Klaim eksternal wajib bersumber & bertahun; prioritaskan sumber resmi/regulator/dokumentasi platform.
Setiap temuan & rekomendasi harus kuantitatif (Rupiah, %, target). "Hasil" iklan = chat WA, bukan penjualan — ROAS tak dapat dihitung.
Akhiri output dengan "Keyakinan: tinggi|sedang|rendah" + alasan.`

// loadDeepSkills reads all *.md files from the skill directory (sorted by
// name) and returns the concatenated text plus the file names. Files are small
// and rarely change, so reading per-request keeps them hot-editable.
func loadDeepSkills() (text string, names []string) {
	for _, dir := range deepSkillDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		files := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				files = append(files, e.Name())
			}
		}
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		var b strings.Builder
		for _, f := range files {
			body, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				continue
			}
			b.WriteString("\n--- " + f + " ---\n")
			b.Write(body)
			b.WriteString("\n")
			names = append(names, f)
		}
		text = b.String()
		if len(text) > maxSkillChars {
			text = text[:maxSkillChars] + " …(dipotong)"
		}
		if len(names) > 0 {
			return text, names
		}
	}
	return fallbackSkill, nil
}

// aiDeepSkills lists the loaded skill files so the UI can show what governs
// the analysis (and admins can verify the directory is picked up).
func (h *Handler) aiDeepSkills(w http.ResponseWriter, _ *http.Request) {
	_, names := loadDeepSkills()
	writeJSON(w, http.StatusOK, struct {
		Skills   []string `json:"skills"`
		Fallback bool     `json:"fallback"`
	}{Skills: names, Fallback: len(names) == 0})
}

// ---- Planner ----

// deepPlannedAgent is one research agent designed by the planner. Riset holds
// suggested web-search queries the agent should start from.
type deepPlannedAgent struct {
	Key        string   `json:"key"`
	Title      string   `json:"title"`
	Icon       string   `json:"icon"`
	Peranan    string   `json:"peranan"`
	Kompetensi string   `json:"kompetensi"`
	Instruksi  string   `json:"instruksi"`
	Output     string   `json:"output"`
	Riset      []string `json:"riset"`
}

type deepPlanRequest struct {
	Model string          `json:"model"`
	Ads   json.RawMessage `json:"ads"`
	Focus string          `json:"focus"` // optional: user's research question
}

type deepPlanResponse struct {
	Agents   []deepPlannedAgent `json:"agents"`
	Skills   []string           `json:"skills"`
	Fallback bool               `json:"fallback"`
}

// defaultDeepPlan is the resilient fallback research panel.
func defaultDeepPlan() []deepPlannedAgent {
	return []deepPlannedAgent{
		{Key: "funnel", Title: "Analis Performa & Funnel", Icon: "📈",
			Peranan:    "Media buyer senior Meta Ads properti residensial Indonesia.",
			Kompetensi: "Efisiensi Spend→Impressions→Reach→Clicks→Hasil (chat WA); CPR/CPC/CPM/CTR.",
			Instruksi:  "Analisis campaign pemenang vs boros dari data internal; hitung selisih vs baseline internal (CPR ≤ Rp 40-60rb, CTR ≥ 0.7%).",
			Output:     "Temuan → Bukti → Implikasi → Rekomendasi berpoin, dengan angka. Maks 220 kata."},
		{Key: "benchmark", Title: "Riset Benchmark Industri", Icon: "🌐",
			Peranan:    "Analis riset pasar digital advertising.",
			Kompetensi: "Mencari & menilai benchmark CTR/CPM/CPC/CPL real estate terbaru dari sumber kredibel.",
			Instruksi:  "Cari benchmark eksternal Meta Ads untuk real estate (utamakan Indonesia/Asia, sebut tahun), lalu bandingkan dengan angka internal.",
			Output:     "Tabel ringkas benchmark vs aktual + implikasi, dengan sitasi. Maks 220 kata.",
			Riset:      []string{"Meta Ads benchmark real estate CTR CPM 2026", "biaya iklan properti Facebook Instagram Indonesia 2026"}},
		{Key: "pasar", Title: "Riset Pasar Properti", Icon: "🏘",
			Peranan:    "Analis pasar properti residensial Indonesia.",
			Kompetensi: "Tren permintaan rumah tapak, KPR/suku bunga, area Jabodetabek.",
			Instruksi:  "Cari kondisi pasar terkini (suku bunga KPR, tren pencarian rumah, insentif pemerintah) dan kaitkan dengan strategi iklan Greenpark.",
			Output:     "3-5 temuan pasar bersitasi + implikasi ke targeting/pesan iklan. Maks 220 kata.",
			Riset:      []string{"suku bunga KPR Indonesia Juli 2026", "tren pasar rumah tapak Jabodetabek 2026"}},
		{Key: "kreatif", Title: "Auditor Kreatif & Copy", Icon: "✍",
			Peranan:    "Creative strategist iklan properti.",
			Kompetensi: "CTR lemah, ad fatigue (frequency), angle/hook copy pasar properti Indonesia.",
			Instruksi:  "Diagnosis kreatif dari sinyal data (CTR rendah, frequency tinggi); riset singkat angle iklan properti yang terbukti, lalu usulkan 3 angle baru.",
			Output:     "Diagnosis + 3 usulan angle konkret (hook + CTA), dengan angka pendukung. Maks 220 kata.",
			Riset:      []string{"contoh iklan properti performa tinggi hook copywriting"}},
		{Key: "aksi", Title: "Strategist Realokasi & Aksi", Icon: "🎯",
			Peranan:    "Ahli alokasi budget & struktur campaign Meta Ads.",
			Kompetensi: "Realokasi ke pemenang, targeting, konsolidasi struktur, exit learning phase.",
			Instruksi:  "Susun rencana realokasi budget spesifik (campaign mana naik/turun/mati, berapa %) berdasarkan data + temuan agent lain.",
			Output:     "Daftar aksi prioritas dengan arah angka & metrik keberhasilan. Maks 220 kata."},
	}
}

// aiDeepPlan lets the AI design the research panel (3–9 agents) for the given
// snapshot, each with suggested web-research queries. Gated by requireAuth.
func (h *Handler) aiDeepPlan(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable, "AI belum dikonfigurasi. Set OLLAMA_API_KEY pada service auth (:8090).")
		return
	}
	var req deepPlanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	skills, skillNames := loadDeepSkills()

	var b strings.Builder
	b.WriteString("Kamu adalah Head of Marketing Greenpark yang memimpin DEEP ANALYSIS: riset mendalam iklan Meta Ads properti (Rupiah) dengan panel agent riset.\n")
	b.WriteString("TUGASMU: merancang panel agent riset PALING RELEVAN untuk data & fokus di bawah. ")
	b.WriteString("KAMU menentukan jumlah agent antara 3 sampai " + strconv.Itoa(maxDeepAgents) + " — sesuaikan kompleksitas data & fokus (data kompleks/fokus luas → panel besar; data sederhana → panel kecil), JANGAN paksakan jumlah tetap.\n")
	b.WriteString("Organisasikan panel dalam beberapa kelompok kerja, misalnya: (1) performa data internal per dimensi (funnel, per-proyek, per-objective), (2) riset eksternal (benchmark industri, pasar properti Jabodetabek, kompetitor, tren musiman), (3) diagnosis kreatif & kualitas lead, (4) strategi aksi & realokasi. Boleh beberapa agent per kelompok bila datanya kaya.\n")
	b.WriteString("Setiap agent bisa MERISET INTERNET (search + buka halaman) — untuk agent yang butuh data eksternal, isi `riset` dengan 1-3 query pencarian spesifik (sertakan tahun & konteks Indonesia/Jabodetabek). Agent yang murni analisis data internal boleh `riset` kosong.\n")
	b.WriteString("Cakup minimal: performa data internal, benchmark/validasi eksternal, dan rekomendasi aksi.\n\n")
	b.WriteString("Balas HANYA JSON valid (tanpa markdown/code fence), bentuk PERSIS:\n")
	b.WriteString(`{"agents":[{"key":"slug-unik","title":"Nama Peran","icon":"satu emoji","peranan":"...","kompetensi":"...","instruksi":"analisis/riset spesifik berbasis angka nyata","output":"format ringkas, maks 220 kata","riset":["query pencarian opsional"]}]}` + "\n")
	b.WriteString("Aturan: 3-" + strconv.Itoa(maxDeepAgents) + " agent; Bahasa Indonesia; JANGAN sertakan agent sintesis (ditangani terpisah).\n")
	b.WriteString("\nSKILL YANG MENGATUR METODOLOGI (patuhi saat merancang instruksi agent):\n" + skills + "\n")

	if focus := strings.TrimSpace(req.Focus); focus != "" {
		if len(focus) > 1500 {
			focus = focus[:1500]
		}
		b.WriteString("\nFOKUS/PERTANYAAN PENGGUNA (panel harus menjawab ini): " + focus + "\n")
	}
	ads := strings.TrimSpace(string(req.Ads))
	if ads != "" && ads != "null" {
		if len(ads) > maxContextChars {
			ads = ads[:maxContextChars] + " …(dipotong)"
		}
		b.WriteString("\nDATA IKLAN META ADS (JSON, sumber kebenaran):\n" + ads + "\n")
	}

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: "Rancang panel agent riset untuk deep analysis ini sekarang. Balas hanya JSON."},
	}
	out, err := h.ai.ChatModel(r.Context(), msgs, strings.TrimSpace(req.Model))
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
		return
	}
	agents := parseDeepPlan(out)
	if len(agents) == 0 {
		writeJSON(w, http.StatusOK, deepPlanResponse{Agents: defaultDeepPlan(), Skills: skillNames, Fallback: true})
		return
	}
	writeJSON(w, http.StatusOK, deepPlanResponse{Agents: agents, Skills: skillNames})
}

// parseDeepPlan extracts and validates the planner JSON, capping at 3–9 agents.
func parseDeepPlan(out string) []deepPlannedAgent {
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		return nil
	}
	var parsed struct {
		Agents []deepPlannedAgent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &parsed); err != nil {
		return nil
	}
	seen := map[string]bool{}
	cleaned := make([]deepPlannedAgent, 0, len(parsed.Agents))
	for idx, a := range parsed.Agents {
		if strings.TrimSpace(a.Instruksi) == "" && strings.TrimSpace(a.Peranan) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(a.Key))
		if key == "" || seen[key] || key == "synthesis" {
			key = "riset" + strconv.Itoa(idx+1)
		}
		seen[key] = true
		a.Key = key
		a.Title = firstNonEmpty(a.Title, "Agent Riset "+strconv.Itoa(idx+1))
		a.Icon = firstNonEmpty(a.Icon, "🔎")
		a.Output = firstNonEmpty(a.Output, "Temuan → Bukti → Implikasi → Rekomendasi berpoin dengan angka. Maks 220 kata.")
		if len(a.Riset) > 3 {
			a.Riset = a.Riset[:3]
		}
		cleaned = append(cleaned, a)
		if len(cleaned) >= maxDeepAgents {
			break
		}
	}
	if len(cleaned) < 3 {
		return nil
	}
	return cleaned
}

// ---- Agent executor with server-side tool loop ----

type deepSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type deepStep struct {
	Tool string `json:"tool"` // search | open
	Arg  string `json:"arg"`  // query or url
	OK   bool   `json:"ok"`
	Note string `json:"note"` // hit count / error / page title
}

type deepAgentRequest struct {
	Agent string          `json:"agent"`
	Model string          `json:"model"`
	Ads   json.RawMessage `json:"ads"`
	Prior json.RawMessage `json:"prior"`
	Focus string          `json:"focus"`
	// Inline frame from the planner (dynamic agents).
	Title      string   `json:"title"`
	Peranan    string   `json:"peranan"`
	Kompetensi string   `json:"kompetensi"`
	Instruksi  string   `json:"instruksi"`
	Output     string   `json:"output"`
	Riset      []string `json:"riset"`
	// For the synthesis finalizer: all sources collected by the researchers.
	Sources []deepSource `json:"sources"`
}

type deepAgentResponse struct {
	Agent   string       `json:"agent"`
	Output  string       `json:"output"`
	Sources []deepSource `json:"sources"`
	Steps   []deepStep   `json:"steps"`
}

// deepAction is the JSON protocol an agent replies with each turn of the loop.
type deepAction struct {
	Tool  string `json:"tool"`  // "search" | "open" | "" (final)
	Query string `json:"query"` // for search
	URL   string `json:"url"`   // for open
	Final string `json:"final"` // final analysis text
}

// parseDeepAction tolerantly extracts the first JSON action object; when the
// model answers in prose instead, the whole text is treated as the final output.
func parseDeepAction(out string) deepAction {
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i >= 0 && j > i {
		var a deepAction
		if err := json.Unmarshal([]byte(out[i:j+1]), &a); err == nil {
			if strings.TrimSpace(a.Final) != "" || strings.TrimSpace(a.Tool) != "" {
				return a
			}
		}
	}
	return deepAction{Final: strings.TrimSpace(out)}
}

// deepSynthesisFrame is the fixed finalizer: merges all researchers' outputs +
// their cited sources into the executive dashboard JSON the frontend renders.
func deepSynthesisFrame() metaAgentFrame {
	return metaAgentFrame{
		title:      "Head of Marketing — Sintesis Deep Analysis",
		peranan:    "Head of Marketing Greenpark yang memimpin seluruh agent riset di atas dan memutuskan untuk direksi.",
		kompetensi: "Menggabungkan analisis data internal + riset eksternal bersitasi menjadi satu dashboard eksekutif yang actionable.",
		instruksi: `Sintesis SEMUA hasil agent riset + data + sumber jadi DASHBOARD EKSEKUTIF DEEP ANALYSIS.
Balas HANYA JSON valid (tanpa markdown/code fence), bentuk:
{
  "title": "judul ringkas",
  "kpis": [ {"label":"...", "value":"...", "note":"konteks singkat", "tone":"ok|warn|bad|neutral"} ],
  "sections": [ {"heading":"...", "items":[ {"title":"...", "detail":"1-2 kalimat, sebut sumber bila dari riset eksternal", "tone":"ok|warn|bad|neutral"} ]} ]
}
Aturan: 4-8 KPI dari angka NYATA (Rupiah bila uang; boleh KPI pembanding "aktual vs benchmark" dari riset); sections WAJIB mencakup "Sorotan", "Temuan Riset & Benchmark" (dengan sitasi sumber), "Risiko & Validasi", "Rekomendasi Prioritas" (dengan target angka); JANGAN mengarang angka atau sumber.`,
		output: "HANYA JSON dashboard sesuai format di atas.",
	}
}

// aiDeepAgent runs ONE deep-analysis agent. Research agents get a bounded
// server-side tool loop (search/open); the synthesis finalizer runs a single
// grounded call. Gated by requireAuth.
func (h *Handler) aiDeepAgent(w http.ResponseWriter, r *http.Request) {
	if h.ai == nil || !h.ai.Configured() {
		writeError(w, http.StatusServiceUnavailable, "AI belum dikonfigurasi. Set OLLAMA_API_KEY pada service auth (:8090).")
		return
	}
	var req deepAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	skills, _ := loadDeepSkills()
	model := strings.TrimSpace(req.Model)

	// ---- Synthesis: single grounded call, no tool loop. ----
	if strings.EqualFold(strings.TrimSpace(req.Agent), "synthesis") {
		ag := deepSynthesisFrame()
		var b strings.Builder
		b.WriteString("Kamu adalah finalizer DEEP ANALYSIS Marketing Greenpark. Bahasa Indonesia, berbasis angka — JANGAN mengarang data/sumber.\n\n")
		writeFrame(&b, ag)
		b.WriteString("\nSKILL (metodologi yang dipatuhi seluruh panel):\n" + skills + "\n")
		writeDeepContext(&b, req, 26000)
		if len(req.Sources) > 0 {
			if sj, err := json.Marshal(req.Sources); err == nil {
				b.WriteString("\nSUMBER YANG DIPAKAI PARA AGENT (untuk sitasi):\n" + string(sj) + "\n")
			}
		}
		msgs := []ai.Message{
			{Role: "system", Content: b.String()},
			{Role: "user", Content: "Kerjakan sintesis sekarang. Balas hanya JSON dashboard."},
		}
		out, err := h.ai.ChatModel(r.Context(), msgs, model)
		if err != nil {
			writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, deepAgentResponse{Agent: req.Agent, Output: out, Sources: req.Sources})
		return
	}

	// ---- Research agent: PKPSICOV frame + bounded tool loop. ----
	if strings.TrimSpace(req.Instruksi) == "" && strings.TrimSpace(req.Peranan) == "" {
		writeError(w, http.StatusBadRequest, "agent tidak dikenal & tidak ada spec dinamis: "+req.Agent)
		return
	}
	ag := metaAgentFrame{
		title:      firstNonEmpty(req.Title, req.Agent, "Agent Riset"),
		peranan:    req.Peranan,
		kompetensi: req.Kompetensi,
		instruksi:  req.Instruksi,
		output:     firstNonEmpty(req.Output, "Temuan → Bukti → Implikasi → Rekomendasi berpoin dengan angka. Maks 220 kata."),
	}

	var b strings.Builder
	b.WriteString("Kamu adalah AGENT RISET dalam pipeline DEEP ANALYSIS Marketing Greenpark (iklan Meta Ads properti, Rupiah). ")
	b.WriteString("Bahasa Indonesia, profesional, berbasis angka — JANGAN mengarang data/sumber.\n\n")
	writeFrame(&b, ag)
	b.WriteString("\nSKILL (WAJIB dipatuhi — metodologi & sumber kredibel):\n" + skills + "\n")
	b.WriteString("\nTOOLS RISET INTERNET — tiap giliran balas HANYA SATU objek JSON (tanpa teks lain):\n")
	b.WriteString(`- Cari web:    {"tool":"search","query":"query spesifik + tahun + Indonesia"}` + "\n")
	b.WriteString(`- Buka halaman: {"tool":"open","url":"https://..."}` + "\n")
	b.WriteString(`- Selesai:      {"final":"analisis lengkapmu sesuai OUTPUT di atas, dengan sitasi (sumber: situs, tahun)"}` + "\n")
	b.WriteString("Hasil tool dikirim balik sebagai pesan berikutnya. Agent murni data internal boleh langsung final.\n")
	if len(req.Riset) > 0 {
		b.WriteString("\nMANDAT RISET EKSTERNAL — kamu WAJIB GIGIH:\n")
		b.WriteString("- JANGAN kirim final sebelum minimal 1 halaman sumber kredibel BERHASIL dibuka (open sukses). Final tanpa sumber akan DITOLAK dan kamu disuruh riset ulang.\n")
		b.WriteString("- Bila query gagal/tidak relevan, JANGAN berhenti — eskalasi bertahap: (1) variasikan istilah, (2) ganti ke bahasa Inggris, (3) perluas cakupan Jabodetabek → Indonesia → Asia Tenggara → global, (4) ganti sudut data (mis. benchmark CPL gagal → cari CPM/CTR industri, laporan pasar properti, data suku bunga/permintaan).\n")
		b.WriteString("- Bila halaman gagal dibuka, buka hasil search lain — jangan menyerah pada 1 URL.\n")
		b.WriteString("- Bila topik utama benar-benar buntu, PIVOT: cari data relevan terdekat yang kredibel dan tetap kaitkan ke tugasmu; sebutkan pivot itu eksplisit di final.\n")
		b.WriteString("Saran query awal dari perencana: " + strings.Join(req.Riset, " | ") + "\n")
	}
	writeDeepContext(&b, req, 6000)

	msgs := []ai.Message{
		{Role: "system", Content: b.String()},
		{Role: "user", Content: "Mulai kerjakan peran \"" + ag.title + "\" sekarang. Balas hanya satu objek JSON."},
	}

	// An agent whose plan includes research queries has an EXTERNAL mandate:
	// it may not finish without at least one successfully opened source.
	needsExternal := len(req.Riset) > 0

	steps := make([]deepStep, 0, maxToolSteps)
	sources := make([]deepSource, 0, 8)
	seenSrc := map[string]bool{}
	final := ""
	toolUsed := 0
	toolBudget := maxToolSteps
	retries := 0

	maxTurns := maxToolSteps + maxRetryRounds*retryExtraSteps + 6
	for turn := 0; turn < maxTurns; turn++ {
		out, err := h.ai.ChatModel(r.Context(), msgs, model)
		if err != nil {
			writeError(w, http.StatusBadGateway, "AI gagal: "+err.Error())
			return
		}
		act := parseDeepAction(out)
		if strings.TrimSpace(act.Final) != "" || act.Tool == "" {
			// Refuse a source-less final from an external-mandate agent: send it
			// back to research with more budget and a new query strategy.
			if needsExternal && len(sources) == 0 && retries < maxRetryRounds {
				retries++
				toolBudget += retryExtraSteps
				msgs = append(msgs,
					ai.Message{Role: "assistant", Content: out},
					ai.Message{Role: "user", Content: "FINAL DITOLAK: kamu belum berhasil membuka SATU PUN sumber, padahal tugasmu butuh data eksternal. " +
						"Riset ulang SEKARANG dengan strategi berbeda (percobaan " + strconv.Itoa(retries) + "/" + strconv.Itoa(maxRetryRounds) + ", budget tool ditambah): " +
						"variasikan istilah → bahasa Inggris → perluas cakupan (Indonesia → Asia → global) → ganti sudut data yang masih relevan & kredibel. " +
						`Balas satu objek JSON {"tool":"search",...}.`})
				continue
			}
			final = firstNonEmpty(strings.TrimSpace(act.Final), strings.TrimSpace(out))
			break
		}
		if toolUsed >= toolBudget {
			msgs = append(msgs,
				ai.Message{Role: "assistant", Content: out},
				ai.Message{Role: "user", Content: `Batas langkah tool tercapai. Balas SEKARANG dengan {"final":"..."} berisi analisis lengkapmu.` +
					" Bila tetap tanpa sumber terbuka, tulis eksplisit strategi pencarian yang sudah dicoba dan data alternatif apa yang disarankan dicari selanjutnya."})
			continue
		}
		toolUsed++
		msgs = append(msgs, ai.Message{Role: "assistant", Content: out})

		switch strings.ToLower(strings.TrimSpace(act.Tool)) {
		case "search":
			results, err := ai.SearchWeb(r.Context(), act.Query, 6)
			if err != nil {
				steps = append(steps, deepStep{Tool: "search", Arg: act.Query, OK: false, Note: err.Error()})
				msgs = append(msgs, ai.Message{Role: "user", Content: "HASIL TOOL search GAGAL: " + err.Error() + "\nJANGAN berhenti — eskalasi: variasikan istilah / bahasa Inggris / perluas cakupan (Indonesia → Asia → global) / ganti sudut data yang masih relevan."})
				continue
			}
			steps = append(steps, deepStep{Tool: "search", Arg: act.Query, OK: true, Note: strconv.Itoa(len(results)) + " hasil"})
			rj, _ := json.Marshal(results)
			body := string(rj)
			if len(body) > 2600 {
				body = body[:2600] + " …(dipotong)"
			}
			msgs = append(msgs, ai.Message{Role: "user", Content: "HASIL TOOL search (JSON):\n" + body + "\nPilih maksimal 1-2 URL paling kredibel untuk dibuka, atau langsung final bila cukup."})
		case "open", "fetch":
			text, err := ai.FetchPage(r.Context(), act.URL, 6000)
			if err != nil {
				steps = append(steps, deepStep{Tool: "open", Arg: act.URL, OK: false, Note: err.Error()})
				msgs = append(msgs, ai.Message{Role: "user", Content: "HASIL TOOL open GAGAL: " + err.Error() + "\nJangan menyerah pada 1 URL — buka hasil search lain atau cari ulang dengan query berbeda."})
				continue
			}
			steps = append(steps, deepStep{Tool: "open", Arg: act.URL, OK: true, Note: strconv.Itoa(len(text)) + " karakter"})
			if !seenSrc[act.URL] {
				seenSrc[act.URL] = true
				sources = append(sources, deepSource{Title: hostOf(act.URL), URL: act.URL})
			}
			msgs = append(msgs, ai.Message{Role: "user", Content: "ISI HALAMAN " + act.URL + " (teks, dipadatkan):\n" + text + "\nEkstrak angka+tahun yang relevan, lalu lanjutkan riset atau balas final."})
		default:
			msgs = append(msgs, ai.Message{Role: "user", Content: `Tool "` + act.Tool + `" tidak dikenal. Gunakan "search", "open", atau {"final":"..."}.`})
		}
	}
	if strings.TrimSpace(final) == "" {
		final = "(agent tidak menyelesaikan analisis dalam batas langkah — pertimbangkan jalankan ulang)"
	}
	writeJSON(w, http.StatusOK, deepAgentResponse{Agent: req.Agent, Output: final, Sources: sources, Steps: steps})
}

// writeFrame appends the PKPSICOV frame shared by all deep-analysis prompts.
func writeFrame(b *strings.Builder, ag metaAgentFrame) {
	b.WriteString("Kerangka PKPSICOV:\n")
	b.WriteString("- PERANAN: " + ag.peranan + "\n")
	b.WriteString("- KOMPETENSI: " + ag.kompetensi + "\n")
	b.WriteString("- SKENARIO: Kadep Marketing Greenpark butuh analisis mendalam iklan Meta (properti Indonesia, Rupiah). Data internal dilampirkan; riset eksternal via tools.\n")
	b.WriteString("- INSTRUKSI: " + ag.instruksi + "\n")
	b.WriteString("- CONSTRAINTS: Angka internal hanya dari data terlampir; klaim eksternal hanya dari halaman yang benar-benar dibuka. Rupiah (IDR). Tandai bila data kosong.\n")
	b.WriteString("- OUTPUT: " + ag.output + "\n")
	b.WriteString("- VALIDATION: \"hasil\" iklan = chat WA (bukan penjualan) → ROAS tak dapat dihitung; pisahkan fakta vs asumsi; akhiri dengan baris Keyakinan.\n")
}

// writeDeepContext appends focus + ads snapshot + prior outputs to the prompt.
func writeDeepContext(b *strings.Builder, req deepAgentRequest, priorCap int) {
	if focus := strings.TrimSpace(req.Focus); focus != "" {
		if len(focus) > 1500 {
			focus = focus[:1500]
		}
		b.WriteString("\nFOKUS/PERTANYAAN PENGGUNA (jawab ini): " + focus + "\n")
	}
	ads := strings.TrimSpace(string(req.Ads))
	if ads != "" && ads != "null" {
		if len(ads) > maxContextChars {
			ads = ads[:maxContextChars] + " …(dipotong)"
		}
		b.WriteString("\nDATA IKLAN META ADS (JSON, sumber kebenaran internal):\n" + ads + "\n")
	}
	prior := strings.TrimSpace(string(req.Prior))
	if prior != "" && prior != "null" && prior != "{}" {
		if len(prior) > priorCap {
			prior = prior[:priorCap] + " …(dipotong)"
		}
		b.WriteString("\nHASIL AGENT SEBELUMNYA (bangun di atasnya, jangan duplikasi):\n" + prior + "\n")
	}
}

// hostOf returns a short display label (host) for a source URL.
func hostOf(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexByte(s, '/'); i > 0 {
		s = s[:i]
	}
	return s
}
