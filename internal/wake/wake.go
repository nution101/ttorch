// Package wake implements the durable wake-queue: the supervisor appends events
// (zero-token), and the manager drains them once per turn. The file is the source
// of truth, so a supervisor restart never loses a pending event.
package wake

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Wake is a single supervision event.
type Wake struct {
	At      time.Time
	Kind    string // signal | stale | check | heartbeat
	Key     string // task id (empty for heartbeat)
	Payload string
}

// Queue is an append-only wake-queue backed by a file.
type Queue struct{ Path string }

// Filter partitions drained wakes for a task-scoped waiter (`ttorch wait --task`):
// matched are the wakes belonging to task (Key == task), rest is every other wake.
// A task-scoped waiter returns matched and must put rest back on the queue so no
// other task's wake is dropped. An empty task matches every wake (plain wait).
func Filter(task string, ws []Wake) (matched, rest []Wake) {
	for _, w := range ws {
		if task == "" || w.Key == task {
			matched = append(matched, w)
		} else {
			rest = append(rest, w)
		}
	}
	return matched, rest
}

func sanitize(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ").Replace(s)
}

// Append records a wake. It is safe for concurrent appends from one process and
// for a separate drainer (drain renames the file away atomically).
func (q Queue) Append(kind, key, payload string) error {
	if err := os.MkdirAll(filepath.Dir(q.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(q.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%d\t%s\t%s\t%s\n", time.Now().UnixNano(), sanitize(kind), sanitize(key), sanitize(payload))
	return err
}

// Drain atomically removes and returns the queued wakes, deduplicated by kind+key
// (all heartbeats collapse to one), preserving first-seen order.
func (q Queue) Drain() ([]Wake, error) {
	tmp := q.Path + ".draining"
	if err := os.Rename(q.Path, tmp); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer os.Remove(tmp)

	b, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	var out []Wake
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		ns, _ := strconv.ParseInt(parts[0], 10, 64)
		w := Wake{At: time.Unix(0, ns), Kind: parts[1], Key: parts[2], Payload: parts[3]}
		dedupe := w.Kind + "\x00" + w.Key
		if w.Kind == "heartbeat" {
			dedupe = "heartbeat"
		}
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		out = append(out, w)
	}
	return out, nil
}
