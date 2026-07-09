// Research tools for the Deep Analysis multi-agent pipeline: a keyless web
// search (DuckDuckGo HTML endpoint) and a page fetcher that reduces HTML to
// readable text. Each deep-analysis agent runs a bounded tool loop on the
// server (see transport/http/ai_deep.go) calling these between LLM turns.
package ai

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SearchResult is one organic web-search hit handed back to the agent.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// toolHTTP is a dedicated client for research tools: shorter timeout than the
// LLM client so a slow site can't stall a whole agent turn.
var toolHTTP = &http.Client{Timeout: 20 * time.Second}

const toolUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) GreenparkDeepAnalysis/1.0"

var (
	// Bing HTML results: each hit is an <li class="b_algo"> with an <h2><a> title.
	reBingLink = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reBingSnip = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	// DuckDuckGo HTML results: title anchor + snippet per result block.
	reDDGLink    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reDDGSnippet = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	reTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	reScript     = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)[^>]*>.*?</\s*(script|style|noscript|svg|head)\s*>`)
	reSpace      = regexp.MustCompile(`[ \t\r\f]+`)
	reBlank      = regexp.MustCompile(`\n{3,}`)
)

// SearchWeb runs a keyless web search, trying providers in order (Bing HTML
// first — reliably reachable; DuckDuckGo HTML as fallback for networks where
// Bing is blocked). Errors surface as text to the agent so the pipeline never
// breaks on a blocked/slow engine.
func SearchWeb(ctx context.Context, query string, max int) ([]SearchResult, error) {
	if max <= 0 {
		max = 6
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("query kosong")
	}
	var lastErr error
	for _, p := range []func(context.Context, string, int) ([]SearchResult, error){searchBing, searchDDG} {
		out, err := p(ctx, q, max)
		if err == nil && len(out) > 0 {
			return out, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("tidak ada hasil")
	}
	return nil, fmt.Errorf("search gagal di semua mesin pencari: %v", lastErr)
}

// searchGET performs one provider request and returns the HTML page.
func searchGET(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", toolUserAgent)
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en;q=0.8")

	res, err := toolHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search: status %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// searchBing scrapes Bing's HTML results (no API key).
func searchBing(ctx context.Context, q string, max int) ([]SearchResult, error) {
	page, err := searchGET(ctx, "https://www.bing.com/search?q="+url.QueryEscape(q)+"&count=10")
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, max)
	for _, chunk := range strings.Split(page, `<li class="b_algo`)[1:] {
		if len(out) >= max {
			break
		}
		m := reBingLink.FindStringSubmatch(chunk)
		if m == nil {
			continue
		}
		href := resolveBingRedirect(html.UnescapeString(m[1]))
		title := cleanText(m[2])
		if href == "" || title == "" || !strings.HasPrefix(href, "http") {
			continue
		}
		snippet := ""
		if sm := reBingSnip.FindStringSubmatch(chunk); sm != nil {
			snippet = cleanText(sm[1])
		}
		out = append(out, SearchResult{Title: title, URL: href, Snippet: snippet})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bing: tidak ada hasil terparse")
	}
	return out, nil
}

// resolveBingRedirect unwraps Bing's /ck/a?…&u=a1<base64url> redirect links.
func resolveBingRedirect(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.Contains(u.Host, "bing.com") && strings.HasPrefix(u.Path, "/ck/") {
		if enc := u.Query().Get("u"); strings.HasPrefix(enc, "a1") {
			if b, err := base64.RawURLEncoding.DecodeString(enc[2:]); err == nil {
				return string(b)
			}
		}
	}
	return href
}

// searchDDG scrapes DuckDuckGo's HTML endpoint (fallback provider).
func searchDDG(ctx context.Context, q string, max int) ([]SearchResult, error) {
	page, err := searchGET(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	links := reDDGLink.FindAllStringSubmatch(page, -1)
	snips := reDDGSnippet.FindAllStringSubmatch(page, -1)

	out := make([]SearchResult, 0, max)
	for i, m := range links {
		if len(out) >= max {
			break
		}
		href := resolveDDGRedirect(html.UnescapeString(m[1]))
		title := cleanText(m[2])
		if href == "" || title == "" {
			continue
		}
		snippet := ""
		if i < len(snips) {
			snippet = cleanText(snips[i][1])
		}
		out = append(out, SearchResult{Title: title, URL: href, Snippet: snippet})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("duckduckgo: tidak ada hasil terparse")
	}
	return out, nil
}

// resolveDDGRedirect unwraps DuckDuckGo's /l/?uddg=<real-url> redirect links.
func resolveDDGRedirect(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.Contains(u.Host, "duckduckgo.com") && u.Query().Get("uddg") != "" {
		return u.Query().Get("uddg")
	}
	return href
}

// FetchPage downloads a URL and reduces it to readable plain text, capped at
// maxChars so one heavy page can't blow the prompt budget.
func FetchPage(ctx context.Context, pageURL string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 6000
	}
	u := strings.TrimSpace(pageURL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("URL harus http(s): %s", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", toolUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en;q=0.8")

	res, err := toolHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: status %s", u, res.Status)
	}
	ct := res.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "text") && !strings.Contains(ct, "json") {
		return "", fmt.Errorf("fetch %s: bukan halaman teks (%s)", u, ct)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}

	text := htmlToText(string(body))
	if text == "" {
		return "", fmt.Errorf("fetch %s: konten kosong setelah dibersihkan", u)
	}
	if len(text) > maxChars {
		text = text[:maxChars] + " …(dipotong)"
	}
	return text, nil
}

// htmlToText strips scripts/styles and tags, unescapes entities, and collapses
// whitespace — a crude but dependency-free readability pass.
func htmlToText(page string) string {
	page = reScript.ReplaceAllString(page, " ")
	// Preserve some block structure as newlines before dropping tags.
	for _, tag := range []string{"</p>", "</div>", "</li>", "</tr>", "</h1>", "</h2>", "</h3>", "<br>", "<br/>", "<br />"} {
		page = strings.ReplaceAll(page, tag, tag+"\n")
	}
	page = reTag.ReplaceAllString(page, " ")
	page = html.UnescapeString(page)
	page = reSpace.ReplaceAllString(page, " ")
	lines := strings.Split(page, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) >= 3 { // drop nav crumbs / empty lines
			kept = append(kept, l)
		}
	}
	out := strings.Join(kept, "\n")
	out = reBlank.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// cleanText strips tags/entities from an HTML fragment into one line.
func cleanText(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reSpace.ReplaceAllString(strings.ReplaceAll(s, "\n", " "), " "))
}
