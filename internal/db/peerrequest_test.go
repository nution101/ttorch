package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAddTaskOncePerRequest proves a task add with a request id creates the row, its created
// event, its brief and its ledger entry once; a repeat of the request id returns the first
// result and writes nothing, whatever it carries; and a request id is never reused for another
// task or another verb.
func TestAddTaskOncePerRequest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	add := func(id, request, brief string) (TaskAddResult, error) {
		return s.AddTask(ctx, TaskAdd{
			Task:      Task{ID: id, ProjectID: proj.ID, Title: "t", Status: StatusPending, Footprint: []string{"a.go"}},
			Actor:     "parent",
			RequestID: request,
			Brief:     brief,
			WriteBrief: func(b string) error {
				written = append(written, b)
				return nil
			},
		})
	}

	first, err := add("peer-1", "req-1", "the brief")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("the brief"))
	if first.Replayed || first.TaskID != "peer-1" || first.ProjectID != proj.ID || first.Status != StatusPending ||
		!first.HasBrief || first.BriefSHA256 != hex.EncodeToString(sum[:]) || first.EventID == 0 || first.CreatedAt.IsZero() {
		t.Fatalf("first add = %+v", first)
	}
	task, ok, err := s.GetTask(ctx, "peer-1")
	if err != nil || !ok || !task.HasBrief || task.CreatedBy != "parent" {
		t.Fatalf("task row = %+v ok=%v err=%v, want has_brief set and created by the actor", task, ok, err)
	}
	if len(written) != 1 || written[0] != "the brief" {
		t.Fatalf("brief written %q, want once", written)
	}
	events := countRows(t, s, `SELECT count(*) FROM events`)

	again, err := add("peer-1", "req-1", "a different brief")
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if !again.Replayed {
		t.Error("a repeat of the request id must say it replayed")
	}
	again.Replayed = false
	if again != first {
		t.Errorf("replayed result = %+v, want the first result %+v", again, first)
	}
	if len(written) != 1 {
		t.Errorf("a repeat wrote the brief again: %q", written)
	}
	if n := countRows(t, s, `SELECT count(*) FROM events`); n != events {
		t.Errorf("a repeat appended %d events", n-events)
	}
	if n := countRows(t, s, `SELECT count(*) FROM peer_requests`); n != 1 {
		t.Errorf("peer_requests rows = %d, want 1", n)
	}

	stored, ok, err := s.StoredTaskAdd(ctx, "req-1")
	if err != nil || !ok || !stored.Replayed {
		t.Fatalf("StoredTaskAdd(req-1) = %+v, %v, %v", stored, ok, err)
	}
	stored.Replayed = false
	if stored != first {
		t.Errorf("StoredTaskAdd = %+v, want the first result %+v", stored, first)
	}
	if _, ok, err := s.StoredTaskAdd(ctx, "req-unknown"); ok || err != nil {
		t.Errorf("StoredTaskAdd of an unknown id = %v, %v; want none", ok, err)
	}

	if _, err := add("peer-2", "req-1", "x"); !errors.Is(err, ErrRequestReused) {
		t.Errorf("a request id reused for another task: want ErrRequestReused, got %v", err)
	}
	if _, ok, _ := s.GetTask(ctx, "peer-2"); ok {
		t.Error("the refused reuse created a task")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO peer_requests (request_id, verb, result, created_at) VALUES ('req-goal', 'goal', '{}', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := add("peer-3", "req-goal", "x"); !errors.Is(err, ErrRequestReused) {
		t.Errorf("a request id stored for another verb: want ErrRequestReused, got %v", err)
	}
	if _, _, err := s.StoredTaskAdd(ctx, "req-goal"); !errors.Is(err, ErrRequestReused) {
		t.Errorf("StoredTaskAdd of a goal's id: want ErrRequestReused, got %v", err)
	}

	if _, err := add("peer-1", "req-2", "x"); !errors.Is(err, ErrTaskExists) {
		t.Errorf("a new request for an existing task: want ErrTaskExists, got %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM peer_requests WHERE request_id = 'req-2'`); n != 0 {
		t.Error("a refused add stored a result")
	}

	// No request id: created, nothing stored in the ledger, and a second add of the id is
	// refused rather than replayed.
	if _, err := add("local-1", "", ""); err != nil {
		t.Fatal(err)
	}
	if local, _, _ := s.GetTask(ctx, "local-1"); local.HasBrief {
		t.Error("an add with no brief set has_brief")
	}
	if _, err := add("local-1", "", ""); !errors.Is(err, ErrTaskExists) {
		t.Errorf("a second add with no request id: want ErrTaskExists, got %v", err)
	}
	if _, err := add("bad-req", "has space", ""); err == nil {
		t.Error("an invalid request id was accepted")
	}
}

// TestAddTaskBriefFailureLeavesNothing proves the brief is part of the add: a WriteBrief error
// rolls back the row, its event and the ledger entry, so a retry of the same request id runs
// the add again instead of replaying a task that has no brief.
func TestAddTaskBriefFailureLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	events := countRows(t, s, `SELECT count(*) FROM events`)
	boom := errors.New("disk full")
	_, err = s.AddTask(ctx, TaskAdd{
		Task:       Task{ID: "t1", ProjectID: proj.ID},
		RequestID:  "req-1",
		Brief:      "b",
		WriteBrief: func(string) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want the brief error, got %v", err)
	}
	if _, ok, _ := s.GetTask(ctx, "t1"); ok {
		t.Error("the row survived a failed brief")
	}
	if n := countRows(t, s, `SELECT count(*) FROM events`); n != events {
		t.Errorf("a failed add left %d events", n-events)
	}
	if n := countRows(t, s, `SELECT count(*) FROM peer_requests`); n != 0 {
		t.Errorf("a failed add left %d ledger rows", n)
	}
	res, err := s.AddTask(ctx, TaskAdd{
		Task:       Task{ID: "t1", ProjectID: proj.ID},
		RequestID:  "req-1",
		Brief:      "b",
		WriteBrief: func(string) error { return nil },
	})
	if err != nil || res.Replayed || !res.HasBrief {
		t.Fatalf("the retry = %+v, %v; want a fresh add", res, err)
	}
	if _, err := s.AddTask(ctx, TaskAdd{Task: Task{ID: "t2", ProjectID: proj.ID}, Brief: "b"}); err == nil {
		t.Error("a brief with no WriteBrief was accepted")
	}
}

// TestAddTaskConcurrentRepeats proves two processes racing the same request id create one task:
// the transaction takes the write lock before it reads the ledger, so the loser replays.
func TestAddTaskConcurrentRepeats(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	stores := make([]*Store, 4)
	for i := range stores {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		stores[i] = s
	}
	proj, err := stores[0].UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		fresh    int
		replayed int
		writes   int
	)
	for _, s := range stores {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			res, err := s.AddTask(ctx, TaskAdd{
				Task:      Task{ID: "race", ProjectID: proj.ID},
				RequestID: "req-race",
				Brief:     "b",
				WriteBrief: func(string) error {
					mu.Lock()
					writes++
					mu.Unlock()
					return nil
				},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("a racing repeat failed: %v", err)
			case res.Replayed:
				replayed++
			default:
				fresh++
			}
		}(s)
	}
	wg.Wait()
	if fresh != 1 || replayed != len(stores)-1 || writes != 1 {
		t.Errorf("fresh %d, replayed %d, brief writes %d; want 1, %d, 1", fresh, replayed, writes, len(stores)-1)
	}
}
