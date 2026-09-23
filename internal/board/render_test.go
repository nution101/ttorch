package board

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

const hostile = `<script>alert("owned")</script>`

// TestWorkerTextRendersInert puts markup into every field that reaches the page from the DB
// or from a worker: a question, a stage, a title, a footprint, a gate finding, a completion
// detail. None of it may arrive as markup; each must arrive as escaped text.
func TestWorkerTextRendersInert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	img := `"><img src=x onerror=alert(1)>`
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive, Stage: hostile})
	h.ask("t1", "should I run this? "+hostile)
	h.addTask(db.Task{ID: "b1", Title: "title " + img, Footprint: []string{"x" + hostile}, HasBrief: true})
	h.addTask(taskDone("g1"))
	h.escalate("g1", `sha=abc adversarial review blocked: "high" ["security"] "`+strings.ReplaceAll(hostile, `"`, `\"`)+`"; "low" "</ul><script>alert(2)</script>"`)
	if _, err := h.store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "d1", Type: db.EventMerged, Actor: db.ActorManager, Payload: hostile,
	}); err != nil {
		t.Fatal(err)
	}

	page, feed := h.page(), h.sections()
	for name, body := range map[string]string{"page": page.body, "sections": feed.body} {
		for _, raw := range []string{hostile, "<script>alert(2)", "<img src=x", "onerror=alert(1)>"} {
			if strings.Contains(body, raw) {
				t.Errorf("%s contains raw markup %q", name, raw)
			}
		}
		if !strings.Contains(body, "&lt;script&gt;alert(&#34;owned&#34;)&lt;/script&gt;") {
			t.Errorf("%s does not show the worker's text escaped", name)
		}
		if !strings.Contains(body, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;") {
			t.Errorf("%s does not show the title escaped", name)
		}
	}
	// The only script on the page is the board's own, and it carries the response's nonce.
	if n := strings.Count(strings.ToLower(feed.body), "<script"); n != 0 {
		t.Errorf("sections feed carries %d script tag(s), want 0", n)
	}
	if n := strings.Count(strings.ToLower(page.body), "<script"); n != 1 {
		t.Errorf("page carries %d script tags, want 1 (its own)", n)
	}
	csp := page.header.Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'nonce-([0-9a-f]+)'`).FindStringSubmatch(csp)
	if m == nil || !strings.Contains(page.body, `<script nonce="`+m[1]+`">`) {
		t.Fatalf("page CSP %q does not pin its own script by nonce", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("page CSP allows inline or eval: %q", csp)
	}
}

func TestSectionsShowEachKindOfDecision(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Mode = func(string) string { return "trusted" }
	h.addTask(db.Task{ID: "live", Window: "wk-live", Status: db.StatusActive, Stage: "writing tests", Footprint: []string{"a.go", "b.go"}})
	h.addTask(db.Task{ID: "asks", Window: "wk-asks", Status: db.StatusActive})
	h.ask("asks", "rebase or merge?")
	h.addTask(db.Task{ID: "queued", Title: "next thing", Footprint: []string{"c.go"}, HasBrief: true})
	h.addTask(db.Task{ID: "nobrief", Title: "unbriefed"})
	h.addTask(taskDone("gated"))
	h.escalate("gated", `sha=abc123 adversarial review blocked: "high" ["security"] "a; b"; "low" ["scope"] "extra file"`)

	snap, err := h.srv.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Workers) != 3 || len(snap.Questions) != 1 || len(snap.Gates) != 1 || len(snap.Backlog) != 2 {
		t.Fatalf("snapshot: %d workers, %d questions, %d gates, %d backlog", len(snap.Workers), len(snap.Questions), len(snap.Gates), len(snap.Backlog))
	}
	g := snap.Gates[0]
	if g.Head != "abc123" || len(g.Findings) != 2 || g.Findings[0] != `"high" ["security"] "a; b"` {
		t.Fatalf("gate decision = %+v", g)
	}
	body := h.sections().body
	for _, want := range []string{
		"rebase or merge?", `data-action="/api/answer"`,
		`data-action="/api/gate-prep"`, "extra file",
		`data-action="/api/dispatch"`, "no stored brief", "a.go, b.go", "writing tests",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sections missing %q", want)
		}
	}
	// One dispatch form: the briefed task has one, the unbriefed one does not.
	if n := strings.Count(body, `data-action="/api/dispatch"`); n != 1 {
		t.Errorf("dispatch forms = %d, want 1", n)
	}
}

func TestApprovalsListOnlyTasksThatNeedTheLead(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	approved := map[string]bool{}
	h.srv.cfg.ApprovalValid = func(id string) bool { return approved[id] }
	mode := "trusted"
	h.srv.cfg.Mode = func(string) string { return mode }

	h.addTask(taskDone("passed-unapproved"))
	if err := h.store.SaveVerdict(ctx, db.Verdict{TaskID: "passed-unapproved", Overall: "pass", ReviewedSHA: "abcdef1234567890", Findings: "[]"}); err != nil {
		t.Fatal(err)
	}
	h.addTask(taskDone("auto-approved"))
	if err := h.store.SaveVerdict(ctx, db.Verdict{TaskID: "auto-approved", Overall: "pass", ReviewedSHA: "1", Findings: "[]", ApprovedBy: "auto", ApprovalSHA: "1"}); err != nil {
		t.Fatal(err)
	}
	h.addTask(taskDone("lapsed"))
	if _, err := h.store.AppendEvent(ctx, db.Event{EntityType: db.EntityTypeTask, EntityID: "lapsed", Type: "approval_required", Actor: db.ActorSystem, Actionable: true, Payload: "auto-approval is 3h old"}); err != nil {
		t.Fatal(err)
	}
	h.addTask(taskDone("ungated")) // trusted, no verdict yet: waiting on the gate, not the lead

	ids := func() map[string]Approval {
		snap, err := h.srv.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Approval{}
		for _, a := range snap.Approvals {
			out[a.TaskID] = a
		}
		return out
	}
	got := ids()
	if len(got) != 2 || got["passed-unapproved"].Command != "ttorch approve passed-unapproved" || got["lapsed"].Reason != "auto-approval is 3h old" {
		t.Fatalf("trusted approvals = %+v", got)
	}

	approved["passed-unapproved"], approved["lapsed"] = true, true
	if got := ids(); len(got) != 0 {
		t.Fatalf("approved tasks still listed: %+v", got)
	}

	// In local mode every done task without an approval needs the lead.
	approved = map[string]bool{"auto-approved": true}
	h.srv.cfg.ApprovalValid = func(id string) bool { return approved[id] }
	mode = "local"
	if got := ids(); len(got) != 3 || got["ungated"].Reason == "" {
		t.Fatalf("local-mode approvals = %+v", got)
	}
	// In pr mode delivery goes through the PR, so nothing is listed.
	mode = "pr"
	if got := ids(); len(got) != 0 {
		t.Fatalf("pr-mode approvals = %+v", got)
	}
}

func TestSplitFindingsRespectsQuotes(t *testing.T) {
	got := splitFindings(`"high" ["sec"] "a; b"; "low" "say \"x; y\" now"; bare`)
	want := []string{`"high" ["sec"] "a; b"`, `"low" "say \"x; y\" now"`, "bare"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("splitFindings = %q, want %q", got, want)
	}
}

func TestRecentCompletionsAreNewestFirstOncePerTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, e := range []struct{ id, typ string }{{"a", db.EventMerged}, {"b", db.EventDelivered}, {"a", db.EventDelivered}} {
		if _, err := h.store.AppendEvent(ctx, db.Event{EntityType: db.EntityTypeTask, EntityID: e.id, Type: e.typ, Actor: db.ActorManager}); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := h.srv.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Completions) != 2 || snap.Completions[0].TaskID != "a" || snap.Completions[1].TaskID != "b" {
		t.Fatalf("completions = %+v", snap.Completions)
	}
}
