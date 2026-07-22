package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AIModel is one entry in the admin-curated AI model catalogue: what the model
// is (name), how capable it is (Intelligence / "kepintaran"), what it's best
// used for (UseCase / "kegunaan"), and a numeric Score. Shown in Panel Admin →
// Model AI; the catalogue is the single source that later drives model choice
// across every AI feature.
type AIModel struct {
	Name         string `json:"name"`         // model id, e.g. "qwen3.5:397b"
	Intelligence string `json:"intelligence"` // kepintaran (deskripsi / level)
	UseCase      string `json:"useCase"`      // kegunaan — lebih untuk apa
	Score        int    `json:"score"`        // 0..100
}

// WithCatalogPersist points the client at a JSON file for the model catalogue so
// admin edits survive restarts. Loads the existing file if present.
func (c *Client) WithCatalogPersist(path string) *Client {
	path = strings.TrimSpace(path)
	if path == "" {
		return c
	}
	c.catalogFile = path
	if b, err := os.ReadFile(path); err == nil {
		var list []AIModel
		if json.Unmarshal(b, &list) == nil {
			c.mu.Lock()
			c.catalog = list
			c.mu.Unlock()
		}
	}
	return c
}

// Catalog returns the curated model catalogue, sorted by score (desc).
func (c *Client) Catalog() []AIModel {
	c.mu.RLock()
	out := make([]AIModel, len(c.catalog))
	copy(out, c.catalog)
	c.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// UpsertCatalogModel adds or replaces an entry (matched by name, case-
// insensitive), persists, and returns the updated catalogue.
func (c *Client) UpsertCatalogModel(m AIModel) []AIModel {
	m.Name = strings.TrimSpace(m.Name)
	m.Intelligence = strings.TrimSpace(m.Intelligence)
	m.UseCase = strings.TrimSpace(m.UseCase)
	c.mu.Lock()
	found := false
	for i := range c.catalog {
		if strings.EqualFold(c.catalog[i].Name, m.Name) {
			c.catalog[i] = m
			found = true
			break
		}
	}
	if !found {
		c.catalog = append(c.catalog, m)
	}
	c.persistCatalogLocked()
	c.mu.Unlock()
	return c.Catalog()
}

// RemoveCatalogModel deletes an entry by name, persists, and returns the rest.
func (c *Client) RemoveCatalogModel(name string) []AIModel {
	name = strings.TrimSpace(name)
	c.mu.Lock()
	kept := c.catalog[:0]
	for _, m := range c.catalog {
		if !strings.EqualFold(m.Name, name) {
			kept = append(kept, m)
		}
	}
	c.catalog = kept
	c.persistCatalogLocked()
	c.mu.Unlock()
	return c.Catalog()
}

// persistCatalogLocked writes the catalogue to disk (caller holds c.mu).
func (c *Client) persistCatalogLocked() {
	if c.catalogFile == "" {
		return
	}
	b, err := json.MarshalIndent(c.catalog, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.catalogFile), 0o755)
	_ = os.WriteFile(c.catalogFile, b, 0o644)
}
