package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DivModel is a division's chosen models: Text (assistant / generate / chat) and
// Vision (baca gambar — perencanaan Deep Revisi). Empty means "fall back to the
// global default set in Panel Admin › Kunci AI".
type DivModel struct {
	Text   string `json:"text"`
	Vision string `json:"vision"`
}

func normDiv(d string) string { return strings.ToLower(strings.TrimSpace(d)) }

// WithDivModelsPersist points the client at a JSON file for per-division model
// choices (loads it if present).
func (c *Client) WithDivModelsPersist(path string) *Client {
	path = strings.TrimSpace(path)
	if path == "" {
		return c
	}
	c.divModelsFile = path
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]DivModel
		if json.Unmarshal(b, &m) == nil && m != nil {
			c.mu.Lock()
			c.divModels = m
			c.mu.Unlock()
		}
	}
	return c
}

// DivisionModel returns a division's stored {text,vision} choice (empty if unset).
func (c *Client) DivisionModel(div string) DivModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.divModels[normDiv(div)]
}

// SetDivisionModel stores + persists a division's model choice.
func (c *Client) SetDivisionModel(div, text, vision string) {
	div = normDiv(div)
	if div == "" {
		return
	}
	c.mu.Lock()
	if c.divModels == nil {
		c.divModels = map[string]DivModel{}
	}
	c.divModels[div] = DivModel{Text: strings.TrimSpace(text), Vision: strings.TrimSpace(vision)}
	c.persistDivModelsLocked()
	c.mu.Unlock()
}

// ModelForDivision resolves the TEXT model a division should use: its own choice
// if set, else the global default model.
func (c *Client) ModelForDivision(div string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m := strings.TrimSpace(c.divModels[normDiv(div)].Text); m != "" {
		return m
	}
	return c.model
}

// VisionModelForDivision resolves the VISION model a division should use: its own
// choice if set, else the global vision model (or the built-in default).
func (c *Client) VisionModelForDivision(div string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m := strings.TrimSpace(c.divModels[normDiv(div)].Vision); m != "" {
		return m
	}
	if c.visionModel != "" {
		return c.visionModel
	}
	return defaultVisionModel
}

func (c *Client) persistDivModelsLocked() {
	if c.divModelsFile == "" {
		return
	}
	b, err := json.MarshalIndent(c.divModels, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.divModelsFile), 0o755)
	_ = os.WriteFile(c.divModelsFile, b, 0o644)
}
