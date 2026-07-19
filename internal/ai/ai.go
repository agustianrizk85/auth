// Package ai is a thin Ollama Cloud (https://ollama.com) chat client used by the
// dashboard's AI features (cross-division assistant, orchestrator, and the Meta
// Ads multi-agent generator). Ollama Cloud exposes an OpenAI-compatible chat
// endpoint, so the request/response shapes mirror the OpenAI spec. It is
// provider-agnostic to the caller: the handler builds the grounded prompt +
// conversation and this package just relays it. When no API key is configured
// the caller surfaces a friendly "AI belum dikonfigurasi" message.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultEndpoint is Ollama Cloud's OpenAI-compatible chat completions URL.
const defaultEndpoint = "https://ollama.com/v1/chat/completions"

// defaultModel is a strong general model available on Ollama Cloud.
const defaultModel = "glm-5.2:cloud"

// defaultVisionModel is a multimodal model used by perencanaan's Deep Revisi AI
// (reads gambar-kerja images). Separate from the general text model above; both
// share the ONE central key.
const defaultVisionModel = "qwen3.5:397b"

// Message is one chat turn (role: system | user | assistant).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client calls Ollama Cloud chat completions. The API key and model are mutable
// at runtime (set from the dashboard UI) and optionally persisted to a file.
type Client struct {
	mu          sync.RWMutex
	key         string
	model       string // general text model (assistant, orchestrator, …)
	visionModel string // multimodal model for perencanaan Deep Revisi
	endpoint    string
	keyFile     string // optional persistence path ("" = in-memory only)
	http        *http.Client
}

// New builds a client. It is always non-nil; Configured() reports usability.
// endpoint may be empty to use Ollama Cloud's default OpenAI-compatible URL.
func New(key, model, endpoint string) *Client {
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultEndpoint
	}
	return &Client{
		key:         strings.TrimSpace(key),
		model:       model,
		visionModel: defaultVisionModel,
		endpoint:    endpoint,
		http:        &http.Client{Timeout: 110 * time.Second},
	}
}

// WithPersist points the client at a file used to persist a UI-set key so it
// survives restarts. If the file already holds a key it overrides the env key.
func (c *Client) WithPersist(path string) *Client {
	path = strings.TrimSpace(path)
	if path == "" {
		return c
	}
	c.keyFile = path
	if b, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			c.mu.Lock()
			c.key = k
			c.mu.Unlock()
		}
	}
	return c
}

// Configured reports whether an API key is present.
func (c *Client) Configured() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.key != ""
}

// Model returns the active general model id.
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// VisionModel returns the model used for multimodal (Deep Revisi) calls.
func (c *Client) VisionModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.visionModel == "" {
		return defaultVisionModel
	}
	return c.visionModel
}

// SetVisionModel updates the vision model at runtime (from the admin UI). Empty
// leaves it unchanged.
func (c *Client) SetVisionModel(m string) {
	if m = strings.TrimSpace(m); m != "" {
		c.mu.Lock()
		c.visionModel = m
		c.mu.Unlock()
	}
}

// SetModel updates the general model WITHOUT touching the key (unlike SetKey).
// Empty leaves it unchanged.
func (c *Client) SetModel(m string) {
	if m = strings.TrimSpace(m); m != "" {
		c.mu.Lock()
		c.model = m
		c.mu.Unlock()
	}
}

// SetKey updates the API key at runtime (and persists it when a key-file is
// configured). An empty model leaves the current one unchanged.
func (c *Client) SetKey(key, model string) {
	key = strings.TrimSpace(key)
	c.mu.Lock()
	c.key = key
	if m := strings.TrimSpace(model); m != "" {
		c.model = m
	}
	keyFile := c.keyFile
	c.mu.Unlock()
	if keyFile != "" {
		_ = os.MkdirAll(filepath.Dir(keyFile), 0o755)
		_ = os.WriteFile(keyFile, []byte(key), 0o600)
	}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	// Ollama returns errors either as {"error":"msg"} (a plain string) or as
	// the OpenAI shape {"error":{"message":"msg"}} — keep raw and parse both.
	Error json.RawMessage `json:"error"`
}

// errMessage extracts a human-readable message from a raw error field that may
// be a string, an object with "message", or arbitrary JSON.
func errMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return string(raw)
}

// Chat sends the conversation (already including the system prompt) to the
// active model and returns the assistant's reply text.
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, error) {
	return c.ChatModel(ctx, msgs, "")
}

// ChatModel is like Chat but overrides the model for this single call when
// modelOverride is non-empty (used for per-run dynamic model selection). The
// client's default model and the configured key are unchanged.
func (c *Client) ChatModel(ctx context.Context, msgs []Message, modelOverride string) (string, error) {
	c.mu.RLock()
	key, model, endpoint := c.key, c.model, c.endpoint
	c.mu.RUnlock()
	if m := strings.TrimSpace(modelOverride); m != "" {
		model = m
	}
	if key == "" {
		return "", fmt.Errorf("Ollama belum dikonfigurasi (set API key)")
	}

	reqBody, _ := json.Marshal(chatRequest{
		Model:       model,
		Temperature: 0.4,
		// Reasoning models (e.g. glm-5.2) spend tokens "thinking" before the
		// visible answer; a low cap truncated the final content to empty. Give
		// enough headroom so the synthesis JSON always completes.
		MaxTokens: 8000,
		Stream:    false,
		Messages:  msgs,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Title", "Greenpark Dashboard AI")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var parsed chatResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("Ollama: gagal baca respons: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := errMessage(parsed.Error)
		if msg == "" {
			msg = "status " + res.Status
		}
		return "", fmt.Errorf("Ollama %d: %s", res.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("Ollama: respons kosong")
	}
	return stripThink(parsed.Choices[0].Message.Content), nil
}

/* ---- Vision (multimodal) — used by perencanaan's Deep Revisi AI proxy ----- */

type visionImageURL struct {
	URL string `json:"url"`
}
type visionPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *visionImageURL `json:"image_url,omitempty"`
}
type visionMessage struct {
	Role    string       `json:"role"`
	Content []visionPart `json:"content"`
}
type visionRequest struct {
	Model       string          `json:"model"`
	Messages    []visionMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens"`
	Stream      bool            `json:"stream"`
}

// CompleteVision sends ONE user message (a text prompt + one or more images) to
// a vision-capable model, using the centrally-configured key. `images` are data
// URLs (data:image/png;base64,…). `model` overrides the client's default text
// model for this call (Deep Revisi passes a vision model). The key never leaves
// this service — callers proxy through here instead of holding the key.
func (c *Client) CompleteVision(ctx context.Context, model, prompt string, images []string) (string, error) {
	c.mu.RLock()
	key, visModel, endpoint := c.key, c.visionModel, c.endpoint
	c.mu.RUnlock()
	if key == "" {
		return "", fmt.Errorf("Ollama belum dikonfigurasi (set API key)")
	}
	if strings.TrimSpace(model) == "" {
		model = visModel
		if model == "" {
			model = defaultVisionModel
		}
	}

	parts := make([]visionPart, 0, len(images)+1)
	parts = append(parts, visionPart{Type: "text", Text: prompt})
	for _, img := range images {
		parts = append(parts, visionPart{Type: "image_url", ImageURL: &visionImageURL{URL: img}})
	}
	reqBody, _ := json.Marshal(visionRequest{
		Model: model, Temperature: 0.3, MaxTokens: 1500, Stream: false,
		Messages: []visionMessage{{Role: "user", Content: parts}},
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Title", "Greenpark Deep Revisi AI")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var parsed chatResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("Ollama: gagal baca respons: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := errMessage(parsed.Error)
		if msg == "" {
			msg = "status " + res.Status
		}
		return "", fmt.Errorf("Ollama %d: %s", res.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("Ollama: respons kosong")
	}
	return stripThink(parsed.Choices[0].Message.Content), nil
}

// stripThink removes <think>…</think> reasoning blocks some models emit inline,
// leaving only the final answer. Tolerates an unclosed tag (drops to end).
func stripThink(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			s = s[:i] // unclosed: drop the rest
			break
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// modelsEndpoint derives the OpenAI-compatible models URL from the chat endpoint
// (…/chat/completions → …/models).
func modelsEndpoint(chat string) string {
	if i := strings.LastIndex(chat, "/chat/completions"); i >= 0 {
		return chat[:i] + "/models"
	}
	return "https://ollama.com/v1/models"
}

// Models lists the model ids available to the configured key (dynamic model
// picker). Returns an error when no key is set.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	c.mu.RLock()
	key, endpoint := c.key, c.endpoint
	c.mu.RUnlock()
	if key == "" {
		return nil, fmt.Errorf("Ollama belum dikonfigurasi (set API key)")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsEndpoint(endpoint), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("Ollama: gagal baca daftar model: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := errMessage(parsed.Error)
		if msg == "" {
			msg = "status " + res.Status
		}
		return nil, fmt.Errorf("Ollama %d: %s", res.StatusCode, msg)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}
