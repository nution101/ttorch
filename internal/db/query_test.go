package db

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// epicIDs/phaseIDs/taskIDs extract surrogate ids for order-sensitive assertions.
func epicIDs(es []Epic) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
func phaseIDs(ps []Phase) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// TestListEpics: ListEpics(0) returns every project's epics grouped by project then
// position then id; a non-zero project scopes to that project.
func TestListEpics(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p1, _ := s.UpsertProject(ctx, "/r1", "r1")
	p2, _ := s.UpsertProject(ctx, "/r2", "r2")
	e1, _ := s.CreateEpic(ctx, p1.ID, "e1", "")
	e2, _ := s.CreateEpic(ctx, p1.ID, "e2", "")
	e3, _ := s.CreateEpic(ctx, p2.ID, "e3", "")

	all, err := s.ListEpics(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := epicIDs(all); !reflect.DeepEqual(got, []int64{e1.ID, e2.ID, e3.ID}) {
		t.Fatalf("ListEpics(0) = %v, want [%d %d %d] (by project then id)", got, e1.ID, e2.ID, e3.ID)
	}
	scoped, err := s.ListEpics(ctx, p1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := epicIDs(scoped); !reflect.DeepEqual(got, []int64{e1.ID, e2.ID}) {
		t.Fatalf("ListEpics(p1) = %v, want [%d %d]", got, e1.ID, e2.ID)
	}
	if empty, _ := s.ListEpics(ctx, 9999); len(empty) != 0 {
		t.Fatalf("ListEpics(missing) = %v, want empty", empty)
	}
}

// TestListPhases: ListPhases(0) groups by epic then id; a non-zero epic scopes.
func TestListPhases(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")
	e1, _ := s.CreateEpic(ctx, p.ID, "e1", "")
	e2, _ := s.CreateEpic(ctx, p.ID, "e2", "")
	ph1, _ := s.CreatePhase(ctx, e1.ID, "ph1", "")
	ph2, _ := s.CreatePhase(ctx, e1.ID, "ph2", "")
	ph3, _ := s.CreatePhase(ctx, e2.ID, "ph3", "")

	all, err := s.ListPhases(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := phaseIDs(all); !reflect.DeepEqual(got, []int64{ph1.ID, ph2.ID, ph3.ID}) {
		t.Fatalf("ListPhases(0) = %v, want [%d %d %d]", got, ph1.ID, ph2.ID, ph3.ID)
	}
	scoped, _ := s.ListPhases(ctx, e1.ID)
	if got := phaseIDs(scoped); !reflect.DeepEqual(got, []int64{ph1.ID, ph2.ID}) {
		t.Fatalf("ListPhases(e1) = %v, want [%d %d]", got, ph1.ID, ph2.ID)
	}
}

// TestListTasksEpicFilter: TaskFilter.EpicID restricts to one epic (a non-zero id
// excludes tasks with NULL epic_id) and composes with the multi-status IN filter.
func TestListTasksEpicFilter(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")
	e1, _ := s.CreateEpic(ctx, p.ID, "e1", "")
	e2, _ := s.CreateEpic(ctx, p.ID, "e2", "")
	mk := func(id string, epic *int64, status string) {
		clk.advance(time.Second)
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: p.ID, EpicID: epic, Status: status}, "manager"); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", &e1.ID, StatusActive)
	mk("b", &e1.ID, StatusBlocked)
	mk("c", &e2.ID, StatusActive)
	mk("d", nil, StatusActive) // no epic — must be excluded by an epic filter

	ids := func(ts []Task) []string {
		out := make([]string, len(ts))
		for i, x := range ts {
			out[i] = x.ID
		}
		return out
	}
	e1only, _ := s.ListTasks(ctx, TaskFilter{EpicID: e1.ID})
	if got := ids(e1only); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("EpicID filter = %v, want [a b]", got)
	}
	// Compose with a multi-status IN filter: only e1 + active.
	e1active, _ := s.ListTasks(ctx, TaskFilter{EpicID: e1.ID, Status: []string{StatusActive}})
	if got := ids(e1active); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("EpicID+Status filter = %v, want [a]", got)
	}
}

// TestGetByID: the by-id getters round-trip and report absence as (zero,false,nil).
func TestGetByID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")
	e, _ := s.CreateEpic(ctx, p.ID, "e", "")
	ph, _ := s.CreatePhase(ctx, e.ID, "ph", "")

	if got, ok, err := s.GetProject(ctx, p.ID); err != nil || !ok || got.RepoPath != "/r" {
		t.Fatalf("GetProject = (%+v,%v,%v)", got, ok, err)
	}
	if got, ok, err := s.GetEpic(ctx, e.ID); err != nil || !ok || got.Title != "e" {
		t.Fatalf("GetEpic = (%+v,%v,%v)", got, ok, err)
	}
	if got, ok, err := s.GetPhase(ctx, ph.ID); err != nil || !ok || got.Title != "ph" {
		t.Fatalf("GetPhase = (%+v,%v,%v)", got, ok, err)
	}
	for _, miss := range []func() (bool, error){
		func() (bool, error) { _, ok, err := s.GetProject(ctx, 9999); return ok, err },
		func() (bool, error) { _, ok, err := s.GetEpic(ctx, 9999); return ok, err },
		func() (bool, error) { _, ok, err := s.GetPhase(ctx, 9999); return ok, err },
	} {
		if ok, err := miss(); ok || err != nil {
			t.Fatalf("missing-id getter = (%v,%v), want (false,nil)", ok, err)
		}
	}
}
