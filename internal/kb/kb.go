// Package kb implements a small, file-backed knowledge base of adopted AI
// failure diagnoses. When an operator accepts a diagnosis, the root cause and
// fix are persisted here so that later, similar failures can be matched and
// shown as a reference without re-calling the model.
package kb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one adopted diagnosis in the knowledge base.
type Entry struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	ProjectName  string `json:"project_name"`
	Stage        string `json:"stage"`        // failing stage name (build / push / deploy)
	Signature    string `json:"signature"`    // dedup key (project + stage + normalized error)
	ErrorKeyword string `json:"error_keyword"` // normalized error text used for similarity match
	ErrorExcerpt string `json:"error_excerpt"` // short human-readable error snippet
	Diagnosis    string `json:"diagnosis"`    // the adopted analysis text
	CreatedAt    string `json:"created_at"`
	AdoptedAt    string `json:"adopted_at"`
	AdoptedCount int    `json:"adopted_count"`
}

// KB is an in-memory knowledge base backed by a JSON file.
type KB struct {
	entries []Entry
	path    string
}

// Load reads the KB from path. A missing file is not an error: it returns an
// empty KB ready to accept entries.
func Load(path string) *KB {
	k := &KB{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return k
	}
	_ = json.Unmarshal(b, &k.entries)
	return k
}

// Save writes the KB to its JSON file (0600). A nil/empty path is a no-op so
// callers can disable persistence by not configuring one.
func (k *KB) Save() error {
	if k.path == "" {
		return nil
	}
	if dir := filepath.Dir(k.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(k.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(k.path, b, 0o600)
}

// Normalize collapses whitespace and lower-cases text so it can be used both as
// a dedup signature component and a case-insensitive similarity key.
func Normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func signatureFor(projectID, stage, errText string) string {
	return projectID + "|" + stage + "|" + Normalize(errText)
}

// Add inserts or merges an entry. Entries with the same signature are merged
// (adopted_count incremented, adopted_at refreshed, and the more detailed
// diagnosis kept), so repeated identical failures reinforce the same KB item.
func (k *KB) Add(e Entry) Entry {
	now := time.Now().Format(time.RFC3339)
	e.Signature = signatureFor(e.ProjectID, e.Stage, e.ErrorKeyword)
	e.ErrorKeyword = Normalize(e.ErrorKeyword)
	for i := range k.entries {
		if k.entries[i].Signature == e.Signature {
			k.entries[i].AdoptedCount++
			k.entries[i].AdoptedAt = now
			if len(e.Diagnosis) > len(k.entries[i].Diagnosis) {
				k.entries[i].Diagnosis = e.Diagnosis
			}
			if e.ErrorExcerpt != "" {
				k.entries[i].ErrorExcerpt = e.ErrorExcerpt
			}
			if e.ProjectName != "" {
				k.entries[i].ProjectName = e.ProjectName
			}
			return k.entries[i]
		}
	}
	e.ID = fmt.Sprintf("kb-%d", time.Now().UnixNano())
	e.CreatedAt = now
	e.AdoptedAt = now
	e.AdoptedCount = 1
	k.entries = append(k.entries, e)
	return e
}

// List returns all entries ordered by most-recently-adopted first.
func (k *KB) List() []Entry {
	out := make([]Entry, len(k.entries))
	copy(out, k.entries)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AdoptedAt > out[j].AdoptedAt
	})
	return out
}

// Remove deletes the entry with the given id. It reports whether an entry was
// actually removed so the caller can 404 on a missing id.
func (k *KB) Remove(id string) bool {
	for i := range k.entries {
		if k.entries[i].ID == id {
			k.entries = append(k.entries[:i], k.entries[i+1:]...)
			return true
		}
	}
	return false
}

// Match returns up to three KB entries whose normalized error keyword appears
// in logs, ordered by how often they have been adopted (battle-tested first).
func (k *KB) Match(logs string) []Entry {
	lower := strings.ToLower(logs)
	var hits []Entry
	for _, e := range k.entries {
		if e.ErrorKeyword != "" && strings.Contains(lower, e.ErrorKeyword) {
			hits = append(hits, e)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].AdoptedCount > hits[j].AdoptedCount
	})
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
}
