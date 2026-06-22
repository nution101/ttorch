// Package learnings is orcha's capture-on-delivery memory loop. The manager records
// durable, project-intrinsic lessons at the approval gate; they accumulate in a
// per-repo ledger (.orcha/learnings.jsonl), and recurring (or pinned) lessons are
// promoted into a capped, always-loaded "Learnings" block in AGENTS.md. Deterministic
// and file-based — no embeddings, no RAG.
package learnings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// PromoteThreshold is the recurrence count at which an unpinned lesson graduates
	// from the ledger into the always-loaded AGENTS.md block.
	PromoteThreshold = 2
	// BlockCap bounds how many lessons the AGENTS.md block holds (hygiene).
	BlockCap = 20
	// MaxLessonLen truncates an individual lesson (hygiene).
	MaxLessonLen = 240

	markerBegin = "<!-- BEGIN orcha-learnings -->"
	markerEnd   = "<!-- END orcha-learnings -->"
)

// Entry is one accumulated lesson.
type Entry struct {
	Text    string    `json:"text"`
	Glob    string    `json:"glob,omitempty"` // optional path scope
	Count   int       `json:"count"`
	Pinned  bool      `json:"pinned,omitempty"`
	Task    string    `json:"task,omitempty"`
	Updated time.Time `json:"updated"`
}

// Store is the per-repo ledger.
type Store struct{ Dir string } // Dir is the repository root

func (s Store) path() string { return filepath.Join(s.Dir, ".orcha", "learnings.jsonl") }

// Load returns the ledger entries (empty if none yet).
func (s Store) Load() ([]Entry, error) {
	b, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func (s Store) save(entries []Entry) error {
	var b strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return atomicWrite(s.path(), []byte(b.String()))
}

// Add upserts a lesson by normalized text: an existing match increments its count
// (so recurrence is tracked); otherwise a new entry is appended.
func (s Store) Add(text, glob, task string, pin bool) (Entry, error) {
	text = truncate(strings.TrimSpace(text))
	entries, err := s.Load()
	if err != nil {
		return Entry{}, err
	}
	key := normalize(text)
	for i := range entries {
		if normalize(entries[i].Text) == key {
			entries[i].Count++
			entries[i].Updated = time.Now()
			if task != "" {
				entries[i].Task = task
			}
			if glob != "" {
				entries[i].Glob = glob
			}
			if pin {
				entries[i].Pinned = true
			}
			return entries[i], s.save(entries)
		}
	}
	e := Entry{Text: text, Glob: glob, Count: 1, Pinned: pin, Task: task, Updated: time.Now()}
	entries = append(entries, e)
	return e, s.save(entries)
}

// Prune drops unpinned one-off lessons older than maxAge, returning how many it removed.
func (s Store) Prune(maxAge time.Duration) (int, error) {
	var removed int
	err := s.withLock(func() error {
		entries, err := s.Load()
		if err != nil {
			return err
		}
		cutoff := time.Now().Add(-maxAge)
		kept := entries[:0]
		for _, e := range entries {
			if !e.Pinned && e.Count <= 1 && e.Updated.Before(cutoff) {
				removed++
				continue
			}
			kept = append(kept, e)
		}
		if removed == 0 {
			return nil
		}
		return s.save(kept)
	})
	return removed, err
}

// withLock serializes ledger read-modify-write across concurrent orcha processes
// (parallel workers/manager) using a mkdir-based lock, so updates can't clobber.
func (s Store) withLock(fn func() error) error {
	dir := filepath.Join(s.Dir, ".orcha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lp := filepath.Join(dir, ".learnings.lock")
	for i := 0; i < 100; i++ {
		if err := os.Mkdir(lp, 0o755); err == nil {
			defer os.Remove(lp)
			return fn()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("could not acquire learnings lock")
}

// Promoted returns the entries that belong in the AGENTS.md block (pinned, or recurring
// past the threshold), sorted and capped.
func Promoted(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Pinned || e.Count >= PromoteThreshold {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned // pinned first
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Updated.After(out[j].Updated)
	})
	if len(out) > BlockCap {
		out = out[:BlockCap]
	}
	return out
}

// Render builds the AGENTS.md block body from the promoted lessons.
func Render(entries []Entry) string {
	promoted := Promoted(entries)
	var b strings.Builder
	b.WriteString("Maintained by orcha — recurring lessons learned in this repo. Apply them.\n\n")
	if len(promoted) == 0 {
		b.WriteString("_No recurring lessons recorded yet._\n")
		return b.String()
	}
	for _, e := range promoted {
		b.WriteString("- " + e.Text)
		if e.Glob != "" {
			b.WriteString("  _(applies to " + e.Glob + ")_")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Apply records a lesson and refreshes the AGENTS.md learnings block, under a lock so
// concurrent captures (parallel workers) can't clobber the ledger or the block.
func Apply(repoDir, text, glob, task string, pin bool) (Entry, error) {
	s := Store{Dir: repoDir}
	var e Entry
	err := s.withLock(func() error {
		var err error
		e, err = s.Add(text, glob, task, pin)
		if err != nil {
			return err
		}
		entries, err := s.Load()
		if err != nil {
			return err
		}
		return writeBlock(filepath.Join(repoDir, "AGENTS.md"), Render(entries))
	})
	return e, err
}

func writeBlock(agentsPath, body string) error {
	block := markerBegin + "\n" + strings.TrimRight(body, "\n") + "\n" + markerEnd
	existing, err := os.ReadFile(agentsPath)
	if os.IsNotExist(err) {
		return atomicWrite(agentsPath, []byte("# Project guidance\n\n"+block+"\n"))
	}
	if err != nil {
		return err
	}
	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi >= 0 && ei > bi {
		return atomicWrite(agentsPath, []byte(text[:bi]+block+text[ei+len(markerEnd):]))
	}
	sep := "\n"
	if !strings.HasSuffix(text, "\n") {
		sep = "\n\n"
	}
	return atomicWrite(agentsPath, []byte(text+sep+block+"\n"))
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func truncate(s string) string {
	if len(s) <= MaxLessonLen {
		return s
	}
	return strings.TrimSpace(s[:MaxLessonLen]) + "…"
}

func atomicWrite(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".orcha-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
