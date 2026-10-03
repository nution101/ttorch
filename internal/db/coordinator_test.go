package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestProvisionAsPeer records a parent on the coordinator row: a root becomes a peer under the
// parent that provisioned it, the same parent may provision it again, another parent is refused
// and changes nothing unless forced, and a forced move reports the parent it replaced.
func TestProvisionAsPeer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	root, err := s.GetCoordinator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parent := strings.Repeat("a", 32)
	other := strings.Repeat("b", 32)

	got, err := s.ProvisionAsPeer(ctx, "build-1", parent, false)
	if err != nil {
		t.Fatalf("first provisioning: %v", err)
	}
	c := got.Coordinator
	if c.CoordID != root.CoordID || c.Role != CoordinatorPeer || c.Name != "build-1" || c.ParentID != parent || got.PreviousParent != "" {
		t.Errorf("after provisioning: %+v, want the same coord id, role peer, name build-1, parent %s, no previous parent", got, parent)
	}

	// The same parent again, under another name: a retry, or the parent re-registering it.
	got, err = s.ProvisionAsPeer(ctx, "build-2", parent, false)
	if err != nil {
		t.Fatalf("the same parent again: %v", err)
	}
	if got.Coordinator.Name != "build-2" || got.Coordinator.ParentID != parent || got.PreviousParent != parent {
		t.Errorf("the same parent again: %+v", got)
	}

	// Another parent is refused, and the row is left as it was.
	if _, err := s.ProvisionAsPeer(ctx, "stolen", other, false); !errors.Is(err, ErrOtherParent) {
		t.Fatalf("another parent without force: err = %v, want ErrOtherParent", err)
	} else if !strings.Contains(err.Error(), parent) {
		t.Errorf("the refusal %q does not name the parent on record", err)
	}
	if c, _ := s.GetCoordinator(ctx); c.ParentID != parent || c.Name != "build-2" {
		t.Errorf("a refused provisioning changed the row: %+v", c)
	}

	// Forced, it moves, and says what it replaced.
	got, err = s.ProvisionAsPeer(ctx, "build-3", other, true)
	if err != nil {
		t.Fatalf("another parent with force: %v", err)
	}
	if got.Coordinator.ParentID != other || got.Coordinator.Name != "build-3" || got.PreviousParent != parent {
		t.Errorf("forced: %+v, want parent %s, previous %s", got, other, parent)
	}

	for _, bad := range []struct{ name, parent string }{
		{"", parent}, {"Build", parent}, {"-b", parent}, {"b c", parent}, {"b/c", parent},
		{strings.Repeat("b", MaxPeerName+1), parent},
		{"b", ""}, {"b", "AAAA"}, {"b", strings.Repeat("g", 32)}, {"b", strings.Repeat("A", 32)},
	} {
		if _, err := s.ProvisionAsPeer(ctx, bad.name, bad.parent, true); err == nil {
			t.Errorf("ProvisionAsPeer(%q, %q) was accepted", bad.name, bad.parent)
		}
	}
	if c, _ := s.GetCoordinator(ctx); c.ParentID != other || c.Name != "build-3" {
		t.Errorf("a refused provisioning changed the row: %+v", c)
	}
}
