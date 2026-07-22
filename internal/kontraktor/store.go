// Package kontraktor is a central, file-backed master list of contractors
// (vendors), shared across every division. Enriched beyond a name so documents
// like the SPK can auto-fill party + bank details. One JSON file holds them all;
// mirrors the klausul store persistence.
package kontraktor

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kontraktor is one vendor/contractor record.
type Kontraktor struct {
	ID       string `json:"id"`
	Nama     string `json:"nama"`
	Jabatan  string `json:"jabatan"`  // mis. Pemborong
	Alamat   string `json:"alamat"`
	Telp     string `json:"telp"`
	Email    string `json:"email"`
	NPWP     string `json:"npwp"`
	Bank     string `json:"bank"`     // mis. BCA
	NoRek    string `json:"noRek"`    // nomor rekening
	AtasNama string `json:"atasNama"` // nama pemegang rekening
	Catatan  string `json:"catatan"`
	Aktif    bool   `json:"aktif"`
}

// Store is a concurrency-safe, file-persisted collection of contractors.
type Store struct {
	mu   sync.RWMutex
	file string
	all  []Kontraktor
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
	var all []Kontraktor
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

// List returns contractors, optionally filtered by a case-insensitive query
// (matches nama/alamat/bank), sorted by Nama.
func (s *Store) List(q string) []Kontraktor {
	q = strings.ToLower(strings.TrimSpace(q))
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Kontraktor, 0, len(s.all))
	for _, k := range s.all {
		if q != "" && !strings.Contains(strings.ToLower(k.Nama+" "+k.Alamat+" "+k.Bank), q) {
			continue
		}
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Nama) < strings.ToLower(out[j].Nama)
	})
	return out
}

// Get returns one contractor by ID.
func (s *Store) Get(id string) (Kontraktor, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.all {
		if k.ID == id {
			return k, true
		}
	}
	return Kontraktor{}, false
}

// Upsert creates (empty ID) or replaces (matching ID) a contractor. Nama required.
func (s *Store) Upsert(k Kontraktor) (Kontraktor, error) {
	k.Nama = strings.TrimSpace(k.Nama)
	if k.Nama == "" {
		return Kontraktor{}, errors.New("nama kontraktor wajib diisi")
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
	k.ID = "kt" + strconv.Itoa(s.seq)
	s.all = append(s.all, k)
	s.persistLocked()
	return k, nil
}

// Delete removes a contractor by ID (no-op if absent).
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Kontraktor, 0, len(s.all))
	for _, k := range s.all {
		if k.ID != id {
			out = append(out, k)
		}
	}
	s.all = out
	s.persistLocked()
}

// seqOf parses the numeric suffix of an ID like "kt42" → 42 (0 if malformed).
func seqOf(id string) int {
	if !strings.HasPrefix(id, "kt") {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "kt"))
	return n
}
