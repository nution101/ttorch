package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testPeer(name string) Peer {
	return Peer{Name: name, ControlDest: "ttorch@build-host", ApproveDest: "lead@build-host", ControlKey: "/keys/" + name}
}

// TestPeerRegistry walks a peer through its life on the parent: registered while provisioning,
// re-registered by a retried add, live once its key answers, refused a second registration while
// in use unless replaced (adopt), retired, and registered again under the same name.
func TestPeerRegistry(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStoreClock(t)

	p, err := s.RegisterPeer(ctx, testPeer("build"), false)
	if err != nil {
		t.Fatal(err)
	}
	created := p.CreatedAt
	if p.Status != PeerProvisioning || p.ControlDest != "ttorch@build-host" || p.ApproveDest != "lead@build-host" ||
		p.ControlKey != "/keys/build" || p.Summary != "{}" || !p.LastOKAt.IsZero() || created.IsZero() {
		t.Errorf("registered = %+v", p)
	}

	// A retried add while still provisioning replaces the destinations and the key.
	clock.advance(time.Minute)
	retry := testPeer("build")
	retry.ControlDest = "ttorch@build-host-2"
	if p, err = s.RegisterPeer(ctx, retry, false); err != nil || p.ControlDest != "ttorch@build-host-2" || p.Status != PeerProvisioning || !p.CreatedAt.Equal(created) {
		t.Fatalf("retried registration = %+v, %v", p, err)
	}

	clock.advance(time.Minute)
	if err := s.MarkPeerLive(ctx, "build", 1, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.GetPeer(ctx, "build")
	if err != nil || !ok {
		t.Fatalf("GetPeer = %v, %v", ok, err)
	}
	if p.Status != PeerLive || p.Protocol != 1 || p.Version != "v1.2.3" || !p.LastOKAt.Equal(clock.now()) || p.LastError != "" {
		t.Errorf("live = %+v", p)
	}

	if _, err := s.RegisterPeer(ctx, testPeer("build"), false); !errors.Is(err, ErrPeerExists) {
		t.Errorf("registering a live peer again: err = %v, want ErrPeerExists", err)
	}

	// Errors are recorded capped; a good call clears them and caches the summary.
	if err := s.RecordPeerError(ctx, "build", strings.Repeat("e", 3*MaxEscalationText)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = s.GetPeer(ctx, "build"); len(p.LastError) > MaxEscalationText || p.Status != PeerLive {
		t.Errorf("after an error: %d bytes of last_error, status %s", len(p.LastError), p.Status)
	}
	clock.advance(time.Minute)
	if err := s.RecordPeerOK(ctx, "build", `{"schema_version":2}`); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = s.GetPeer(ctx, "build"); p.LastError != "" || p.Summary != `{"schema_version":2}` || !p.LastOKAt.Equal(clock.now()) {
		t.Errorf("after a good call: %+v", p)
	}

	// adopt replaces a live peer's registration and starts it provisioning again.
	if p, err = s.RegisterPeer(ctx, testPeer("build"), true); err != nil || p.Status != PeerProvisioning {
		t.Fatalf("replacing a live peer = %+v, %v", p, err)
	}
	if err := s.MarkPeerLive(ctx, "build", 1, "v1.2.4"); err != nil {
		t.Fatal(err)
	}

	if p, err = s.RetirePeer(ctx, "build"); err != nil || p.Status != PeerRetired {
		t.Fatalf("retire = %+v, %v", p, err)
	}
	if p, err = s.RetirePeer(ctx, "build"); err != nil || p.Status != PeerRetired {
		t.Fatalf("retiring twice = %+v, %v", p, err)
	}
	if err := s.MarkPeerLive(ctx, "build", 1, "v1"); err == nil {
		t.Error("a retired peer was marked live")
	}
	if p, err = s.RegisterPeer(ctx, testPeer("build"), false); err != nil || p.Status != PeerProvisioning || !p.CreatedAt.Equal(created) {
		t.Fatalf("registering a retired name again = %+v, %v", p, err)
	}

	if _, err := s.RegisterPeer(ctx, testPeer("alpha"), false); err != nil {
		t.Fatal(err)
	}
	peers, err := s.ListPeers(ctx)
	if err != nil || len(peers) != 2 || peers[0].Name != "alpha" || peers[1].Name != "build" {
		t.Errorf("ListPeers = %+v, %v; want alpha then build", peers, err)
	}

	if _, ok, err := s.GetPeer(ctx, "nobody"); ok || err != nil {
		t.Errorf("GetPeer(nobody) = %v, %v", ok, err)
	}
	for _, f := range []func() error{
		func() error { return s.MarkPeerLive(ctx, "nobody", 1, "v") },
		func() error { return s.RecordPeerOK(ctx, "nobody", "{}") },
		func() error { return s.RecordPeerError(ctx, "nobody", "x") },
		func() error { _, err := s.RetirePeer(ctx, "nobody"); return err },
	} {
		if err := f(); !errors.Is(err, ErrPeerNotFound) {
			t.Errorf("an update to an unregistered peer: err = %v, want ErrPeerNotFound", err)
		}
	}
	for _, bad := range []Peer{
		{Name: "Bad Name", ControlDest: "d", ApproveDest: "d", ControlKey: "/k"},
		{Name: "ok", ControlDest: "", ApproveDest: "d", ControlKey: "/k"},
		{Name: "ok", ControlDest: "d", ApproveDest: "", ControlKey: "/k"},
		{Name: "ok", ControlDest: "d", ApproveDest: "d", ControlKey: "relative"},
	} {
		if _, err := s.RegisterPeer(ctx, bad, false); err == nil {
			t.Errorf("RegisterPeer(%+v) was accepted", bad)
		}
	}
}

// TestPeerRepos: one repository belongs to one coordinator. A peer's repo is refused when this
// coordinator has a project with the same origin, in any of the usual spellings, or another peer
// in use owns it; a retired peer or an archived project gives it up.
func TestPeerRepos(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, name := range []string{"build", "other", "old"} {
		if _, err := s.RegisterPeer(ctx, testPeer(name), false); err != nil {
			t.Fatal(err)
		}
	}
	proj, err := s.UpsertProject(ctx, "/local/app", "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefaultBranch(ctx, proj.ID, "main", "git@example.com:org/app.git"); err != nil {
		t.Fatal(err)
	}

	r, err := s.AddPeerRepo(ctx, "build", "/srv/lib", "https://example.com/org/lib.git")
	if err != nil || r.Peer != "build" || r.RemotePath != "/srv/lib" || r.OriginURL != "https://example.com/org/lib.git" {
		t.Fatalf("AddPeerRepo = %+v, %v", r, err)
	}
	if _, err := s.AddPeerRepo(ctx, "build", "/srv/lib", "https://example.com/org/lib.git"); err != nil {
		t.Errorf("the same repo again: %v, want it accepted as it is", err)
	}
	if _, err := s.AddPeerRepo(ctx, "build", "/srv/lib", "https://example.com/org/elsewhere.git"); err == nil {
		t.Error("the same path with another origin was accepted")
	}

	for _, origin := range []string{
		"git@example.com:org/app.git", "git@example.com:org/app", "https://example.com/org/app",
		"ssh://git@example.com/org/app.git", "https://EXAMPLE.com/org/app.git/", " git@example.com:org/app.git ",
	} {
		if _, err := s.AddPeerRepo(ctx, "build", "/srv/app", origin); !errors.Is(err, ErrRepoOwned) {
			t.Errorf("a parent project's origin as %q: err = %v, want ErrRepoOwned", origin, err)
		}
	}
	if _, err := s.AddPeerRepo(ctx, "other", "/elsewhere/lib", "git@example.com:org/lib"); !errors.Is(err, ErrRepoOwned) {
		t.Errorf("another peer's origin: err = %v, want ErrRepoOwned", err)
	}
	if _, err := s.AddPeerRepo(ctx, "build", "/srv/lib-2", "git@example.com:org/lib"); !errors.Is(err, ErrRepoOwned) {
		t.Errorf("the same origin at a second path on one peer: err = %v, want ErrRepoOwned", err)
	}

	if _, err := s.AddPeerRepo(ctx, "old", "/srv/tool", "https://example.com/org/tool.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetirePeer(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPeerRepo(ctx, "other", "/srv/tool", "https://example.com/org/tool.git"); err != nil {
		t.Errorf("a retired peer's repo: %v, want it free", err)
	}
	if _, err := s.AddPeerRepo(ctx, "old", "/srv/x", "https://example.com/org/x.git"); err == nil {
		t.Error("a repo was added to a retired peer")
	}
	if _, err := s.AddPeerRepo(ctx, "nobody", "/srv/x", "https://example.com/org/x.git"); !errors.Is(err, ErrPeerNotFound) {
		t.Errorf("an unregistered peer: err = %v, want ErrPeerNotFound", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET status = 'archived' WHERE id = ?`, proj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPeerRepo(ctx, "build", "/srv/app", "git@example.com:org/app.git"); err != nil {
		t.Errorf("an archived project's origin: %v, want it free", err)
	}
	for _, bad := range [][2]string{{"", "o"}, {"relative/path", "o"}, {"/srv/y", ""}, {"/srv/y\n", "o"}} {
		if _, err := s.AddPeerRepo(ctx, "build", bad[0], bad[1]); err == nil {
			t.Errorf("AddPeerRepo(%q, %q) was accepted", bad[0], bad[1])
		}
	}

	repos, err := s.ListPeerRepos(ctx, "build")
	if err != nil || len(repos) != 2 || repos[0].RemotePath != "/srv/app" || repos[1].RemotePath != "/srv/lib" {
		t.Errorf("ListPeerRepos(build) = %+v, %v", repos, err)
	}
}

// TestPeerDelegations: a delegation is recorded under its request id once; a repeat of the id
// for the same request says so, and one for anything else is refused. A refused request's record
// can be dropped.
func TestPeerDelegations(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStoreClock(t)
	for _, name := range []string{"build", "other"} {
		if _, err := s.RegisterPeer(ctx, testPeer(name), false); err != nil {
			t.Fatal(err)
		}
	}
	task := Delegation{RequestID: "pa-1", Peer: "build", Kind: DelegationTask, RemoteTaskID: "t-1", BriefSHA256: strings.Repeat("a", 64)}
	got, existed, err := s.RecordDelegation(ctx, task)
	if err != nil || existed || got.CreatedAt.IsZero() {
		t.Fatalf("RecordDelegation = %+v, %v, %v", got, existed, err)
	}
	clock.advance(time.Second)
	if again, existed, err := s.RecordDelegation(ctx, task); err != nil || !existed || !again.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("the same delegation again = %+v, %v, %v; want the first, existing", again, existed, err)
	}
	for _, changed := range []Delegation{
		{RequestID: "pa-1", Peer: "other", Kind: DelegationTask, RemoteTaskID: "t-1", BriefSHA256: task.BriefSHA256},
		{RequestID: "pa-1", Peer: "build", Kind: DelegationGoal, BriefSHA256: task.BriefSHA256},
		{RequestID: "pa-1", Peer: "build", Kind: DelegationTask, RemoteTaskID: "t-2", BriefSHA256: task.BriefSHA256},
		{RequestID: "pa-1", Peer: "build", Kind: DelegationTask, RemoteTaskID: "t-1", BriefSHA256: strings.Repeat("b", 64)},
	} {
		if _, _, err := s.RecordDelegation(ctx, changed); !errors.Is(err, ErrRequestReused) {
			t.Errorf("request id reused for %+v: err = %v, want ErrRequestReused", changed, err)
		}
	}
	goal := Delegation{RequestID: "pg-1", Peer: "build", Kind: DelegationGoal, BriefSHA256: strings.Repeat("c", 64)}
	if _, _, err := s.RecordDelegation(ctx, goal); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Delegation{
		{RequestID: "", Peer: "build", Kind: DelegationGoal},
		{RequestID: "x y", Peer: "build", Kind: DelegationGoal},
		{RequestID: "p-2", Peer: "nobody", Kind: DelegationGoal},
		{RequestID: "p-3", Peer: "build", Kind: "answer"},
		{RequestID: "p-4", Peer: "build", Kind: DelegationTask},
		{RequestID: "p-5", Peer: "build", Kind: DelegationGoal, RemoteTaskID: "t-9"},
	} {
		if _, _, err := s.RecordDelegation(ctx, bad); err == nil {
			t.Errorf("RecordDelegation(%+v) was accepted", bad)
		}
	}

	list, err := s.ListDelegations(ctx, "build")
	if err != nil || len(list) != 2 || list[0].RequestID != "pa-1" || list[1].RequestID != "pg-1" {
		t.Errorf("ListDelegations = %+v, %v", list, err)
	}
	counts, err := s.CountDelegations(ctx)
	if err != nil || counts["build"] != (DelegationCount{Tasks: 1, Goals: 1}) || counts["other"] != (DelegationCount{}) {
		t.Errorf("CountDelegations = %+v, %v", counts, err)
	}

	if err := s.ForgetDelegation(ctx, "pg-1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListDelegations(ctx, "build"); len(list) != 1 {
		t.Errorf("after forgetting one: %+v", list)
	}
	if err := s.ForgetDelegation(ctx, "pg-1"); err != nil {
		t.Errorf("forgetting a delegation twice: %v", err)
	}
}
