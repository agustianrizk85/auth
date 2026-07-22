// Package klausul is a central, file-backed library of reusable clause
// templates ("master klausul"), shared by every division. A clause belongs to a
// division and a document type (e.g. Teknik → "SPK") and its Body may contain
// {placeholder} tokens filled at compose time. One JSON file holds them all;
// reads filter by division/docType. Mirrors the ai model-catalog persistence.
package klausul

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Klausul is one reusable clause template.
type Klausul struct {
	ID       string `json:"id"`
	Division string `json:"division"` // slug: teknik, legalpermit, sales…
	DocType  string `json:"docType"`  // SPK, PKS, BAST…
	Code     string `json:"code"`     // A, B, F … (display label, optional)
	Title    string `json:"title"`    // e.g. "TATA CARA PEMBAYARAN"
	Body     string `json:"body"`     // template text, may contain {placeholder}
	Order    int    `json:"order"`    // sort order within (division, docType)
}

// Store is a concurrency-safe, file-persisted collection of all clauses.
type Store struct {
	mu   sync.RWMutex
	file string
	all  []Klausul
	seq  int
}

// New loads the store from file (missing file = empty store).
func New(file string) *Store {
	s := &Store{file: file}
	s.load()
	return s
}

func (s *Store) load() {
	b, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	var all []Klausul
	if json.Unmarshal(b, &all) != nil {
		return
	}
	s.all = all
	for _, k := range all {
		if n := seqOf(k.ID); n > s.seq {
			s.seq = n
		}
	}
}

func (s *Store) persistLocked() {
	if s.file == "" {
		return
	}
	b, err := json.MarshalIndent(s.all, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.file, b, 0o644)
}

// List returns clauses for a division, optionally narrowed to one docType,
// sorted by docType then Order then Code. Empty division returns everything.
func (s *Store) List(division, docType string) []Klausul {
	division = strings.ToLower(strings.TrimSpace(division))
	docType = strings.TrimSpace(docType)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Klausul, 0, len(s.all))
	for _, k := range s.all {
		if division != "" && strings.ToLower(k.Division) != division {
			continue
		}
		if docType != "" && !strings.EqualFold(k.DocType, docType) {
			continue
		}
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].DocType, out[j].DocType) {
			return out[i].DocType < out[j].DocType
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// Get returns one clause by ID.
func (s *Store) Get(id string) (Klausul, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.all {
		if k.ID == id {
			return k, true
		}
	}
	return Klausul{}, false
}

// Upsert creates (empty ID) or replaces (matching ID) a clause. Division,
// docType and title are required.
func (s *Store) Upsert(k Klausul) (Klausul, error) {
	k.Division = strings.ToLower(strings.TrimSpace(k.Division))
	k.DocType = strings.TrimSpace(k.DocType)
	k.Title = strings.TrimSpace(k.Title)
	k.Code = strings.TrimSpace(k.Code)
	if k.Division == "" || k.DocType == "" || k.Title == "" {
		return Klausul{}, errors.New("divisi, jenis dokumen, dan judul wajib diisi")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.ID != "" {
		for i := range s.all {
			if s.all[i].ID == k.ID {
				s.all[i] = k
				s.persistLocked()
				return k, nil
			}
		}
	}
	s.seq++
	k.ID = "k" + strconv.Itoa(s.seq)
	s.all = append(s.all, k)
	s.persistLocked()
	return k, nil
}

// Delete removes a clause by ID (no-op if absent).
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Klausul, 0, len(s.all))
	for _, k := range s.all {
		if k.ID != id {
			out = append(out, k)
		}
	}
	s.all = out
	s.persistLocked()
}

// seqOf parses the numeric suffix of an ID like "k42" → 42 (0 if malformed).
func seqOf(id string) int {
	if !strings.HasPrefix(id, "k") {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "k"))
	return n
}
