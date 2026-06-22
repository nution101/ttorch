package state

import (
	"testing"
	"time"
)

func TestTaskSessionIDRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	want := Task{
		ID: "t1", Window: "wk-t1", Worktree: "/tmp/wt", Project: "/repo",
		Harness: "claude", Kind: "ship", Created: time.Now().Truncate(time.Second),
		SessionID: "abc-123",
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID {
		t.Fatalf("SessionID round-trip: got %q, want %q", got.SessionID, want.SessionID)
	}
}

func TestManagerRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}

	// Absent: LoadManager returns (zero, false, nil).
	if m, ok, err := s.LoadManager(); err != nil || ok || m.Dir != "" {
		t.Fatalf("absent manager: got (%+v, %v, %v), want (zero, false, nil)", m, ok, err)
	}

	want := Manager{Dir: "/work/repo", SessionID: "mgr-sid-1"}
	if err := s.SaveManager(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadManager()
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("manager round-trip: got %+v, want %+v", got, want)
	}

	if err := s.RemoveManager(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadManager(); ok {
		t.Fatal("manager should be gone after RemoveManager")
	}
	// Removing a missing record is a no-op.
	if err := s.RemoveManager(); err != nil {
		t.Fatalf("removing absent manager should be a no-op: %v", err)
	}
}

func TestListSkipsManager(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(Task{ID: "t1", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveManager(Manager{Dir: "/d", SessionID: "x"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("List should return only the task, got %+v", tasks)
	}
}
