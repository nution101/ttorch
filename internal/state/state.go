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
	ID       string    `json:"id"`
	Window   string    `json:"window"`
	Worktree string    `json:"worktree"`
	Project  string    `json:"project"`
	Harness  string    `json:"harness"`
	Kind     string    `json:"kind"` // ship | scout | cc
	Created  time.Time `json:"created"`
	PR       string    `json:"pr,omitempty"`
}

// Store reads and writes task records in a directory.
type Store struct{ Dir string }

const suffix = ".meta.json"

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+suffix) }

// Save writes a task record atomically.
func (s Store) Save(t Task) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
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
	return os.Rename(name, s.path(t.ID))
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

// List returns all task records, oldest first.
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
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
