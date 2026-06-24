// Package state persists task records as JSON under ~/.ttorch/state, so the manager
// is restart-proof: any session can read the current team from disk.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Task is the durable record for one dispatched worker (or attached cc session).
type Task struct {
	ID        string    `json:"id"`
	Window    string    `json:"window"`
	Worktree  string    `json:"worktree"`
	Project   string    `json:"project"`
	Harness   string    `json:"harness"`
	Kind      string    `json:"kind"` // ship | scout | cc
	Created   time.Time `json:"created"`
	PR        string    `json:"pr,omitempty"`
	SessionID string    `json:"sessionId,omitempty"` // stable harness session id for resume
	// Delivery provenance, so a merge no human read can be reconstructed from state.
	// All omitempty and additive: old records load to zero values and re-save
	// byte-identical.
	GatePassed  bool   `json:"gatePassed,omitempty"`  // the adversarial-review verdict passed
	ApprovedBy  string `json:"approvedBy,omitempty"`  // human | auto (trusted-mode auto-approval)
	ReviewedSHA string `json:"reviewedSha,omitempty"` // the commit the verdict was recorded against
	// Footprint is the repo-relative file paths / directory prefixes this task
	// declares it will touch, set via `ttorch spawn --touches`. It makes "do two
	// workers overlap?" computable rather than guesswork, so two tasks are never
	// dispatched onto the same files. Absent (nil) means undeclared: no overlap
	// enforcement, preserving prior behavior. omitempty + additive, so old records
	// load to nil and re-save byte-identical.
	Footprint []string `json:"footprint,omitempty"`
}

// Manager is the durable record for the manager window, so a restore can rebuild
// it in the same directory and resume the same conversation.
type Manager struct {
	Dir       string `json:"dir"`
	SessionID string `json:"sessionId"`
}

// Store reads and writes task records in a directory.
type Store struct{ Dir string }

const suffix = ".meta.json"

// managerFile is the reserved record holding the manager's directory and session
// id. It lives alongside task records but is never returned by List.
const managerFile = "manager.json"

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+suffix) }

func (s Store) managerPath() string { return filepath.Join(s.Dir, managerFile) }

// Save writes a task record atomically.
func (s Store) Save(t Task) error {
	return s.writeJSON(s.path(t.ID), t)
}

// Load reads a single task record.
func (s Store) Load(id string) (Task, error) {
	var t Task
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(b, &t)
	return t, err
}

// Remove deletes a task record.
func (s Store) Remove(id string) error {
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// SaveManager writes the manager record atomically.
func (s Store) SaveManager(m Manager) error {
	return s.writeJSON(s.managerPath(), m)
}

// LoadManager reads the manager record. The bool reports whether the record
// exists; when the file is absent it returns (zero, false, nil).
func (s Store) LoadManager() (Manager, bool, error) {
	var m Manager
	b, err := os.ReadFile(s.managerPath())
	if os.IsNotExist(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, err
	}
	return m, true, nil
}

// RemoveManager deletes the manager record.
func (s Store) RemoveManager() error {
	err := os.Remove(s.managerPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns all task records, oldest first. The reserved manager.json record
// is never returned as a Task.
func (s Store) List() ([]Task, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, e := range entries {
		if e.IsDir() || e.Name() == managerFile || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), suffix)
		if t, err := s.Load(id); err == nil {
			tasks = append(tasks, t)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created.Before(tasks[j].Created) })
	return tasks, nil
}

// writeJSON marshals v and writes it atomically to path (creating the directory).
func (s Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".ttorch-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
