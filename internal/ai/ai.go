// Package ai is a thin OpenRouter (https://openrouter.ai) chat client used by
// the dashboard's cross-division AI assistant. It is provider-agnostic to the
// caller: the handler builds the grounded system prompt + conversation and this
// package just relays it. When no API key is configured the caller surfaces a
// friendly "AI belum dikonfigurasi" message.
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

const endpoint = "https://openrouter.ai/api/v1/chat/completions"

// Message is one chat turn (role: system | user | assistant).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client calls OpenRouter chat completions. The API key and model are mutable
// at runtime (set from the dashboard UI) and optionally persisted to a file.
type Client struct {
	mu      sync.RWMutex
	key     string
	model   string
	site    string
	keyFile string // optional persistence path ("" = in-memory only)
	http    *http.Client
}

const defaultModel = "openai/gpt-oss-120b:free"

// New builds a client. It is always non-nil; Configured() reports usability.
func New(key, model, site string) *Client {
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	if strings.TrimSpace(site) == "" {
		site = "http://localhost"
	}
	return &Client{key: strings.TrimSpace(key), model: model, site: site, http: &http.Client{Timeout: 110 * time.Second}}
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

// Model returns the active model id.
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
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
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat sends the conversation (already including the system prompt) to the model
// and returns the assistant's reply text.
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, error) {
	c.mu.RLock()
	key, model := c.key, c.model
	c.mu.RUnlock()
	if key == "" {
		return "", fmt.Errorf("OpenRouter belum dikonfigurasi (set API key)")
	}

	reqBody, _ := json.Marshal(chatRequest{
		Model:       model,
		Temperature: 0.4,
		MaxTokens:   900,
		Messages:    msgs,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("HTTP-Referer", c.site)
	httpReq.Header.Set("X-Title", "Greenpark Dashboard Assistant")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var parsed chatResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("OpenRouter: gagal baca respons: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := "status " + res.Status
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return "", fmt.Errorf("OpenRouter %d: %s", res.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("OpenRouter: respons kosong")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
