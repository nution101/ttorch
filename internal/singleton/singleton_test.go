package singleton

import (
	"path/filepath"
	"testing"
)

// TestAcquireHeldRelease exercises the flock-as-truth singleton: a fresh lock is free, the first
// Acquire wins, a SECOND Acquire loses while the first is held (no double-singleton — the core
// guarantee), and after Release the lock frees so a later Acquire wins again. flock distinguishes
// open file descriptions (not processes), so the contention is observable within this one test
// process.
func TestAcquireHeldRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.pid")

	if Held(path) {
		t.Fatal("a fresh lock must not read as held")
	}
	f, acquired, err := Acquire(path)
	if err != nil || !acquired {
		t.Fatalf("the first Acquire should win: acquired=%v err=%v", acquired, err)
	}
	if !Held(path) {
		t.Fatal("the lock must read as held while a holder keeps it")
	}

	// A second Acquire while the first is held must lose — the singleton guarantee.
	f2, acquired2, err := Acquire(path)
	if err != nil {
		t.Fatalf("the second Acquire errored: %v", err)
	}
	if acquired2 {
		Release(f2)
		t.Fatal("a second Acquire must NOT win while the lock is held (double-singleton)")
	}

	// Release the first holder; the lock frees and a new Acquire wins again.
	Release(f)
	if Held(path) {
		t.Fatal("the lock must be free after Release")
	}
	f3, acquired3, err := Acquire(path)
	if err != nil || !acquired3 {
		t.Fatalf("Acquire after Release should win: acquired=%v err=%v", acquired3, err)
	}
	Release(f3)
}
