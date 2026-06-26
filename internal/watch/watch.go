// Package watch implements `ttorch watch`: the manager-owned, event-driven
// blocking watcher that replaces the supervisor's keystroke poke (§4). Each turn in
// which it is not awaiting the lead, the manager arms `ttorch watch` as a background
// task; when an actionable DB transition occurs the watcher prints the coalesced
// batch and exits, and the harness re-invokes the manager through its own
// background-task-completion channel — no process ever types into the manager
// window (§B.1). The watcher also folds in the retired supervisor's two
// non-injection polls: armed-PR-merge detection and a stale/gone liveness safety
// net (§4.4).
//
// Concurrency discipline (§4.2/§9): every sweep opens and CLOSES its own short read
// via the Store helpers — no read transaction is ever held across the wait, so the
// WAL can be checkpointed and never grows unbounded. Section citations (§…) refer to
// docs/design/sqlite-event-architecture.md.
package watch

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/tmux"
)

// managerWindow is the tmux window the manager session runs in. The watcher self-
// exits when it is absent for too long (§4.5) but never types into it (§0.2).
const managerWindow = "manager"

// Defaults for the watcher's cadence and thresholds. All are overridable on the
// Watcher so tests run deterministically (§8 row 3).
const (
	defaultCoalesce  = 750 * time.Millisecond // §4.3: absorb a burst before surfacing
	defaultPoll      = 1 * time.Second        // steady-state detection interval (§4.2)
	defaultStaleHold = 60 * time.Second       // a task must be quiet this long before liveness inspects its pane (§4.4)
	defaultPRCheck   = 60 * time.Second       // rate-limit on `gh pr view` (was supervisor.checkEvery)
	mgrAbsentSweeps  = 3                      // self-exit after this many consecutive manager-absent sweeps (§4.5)
	captureLines     = 6                      // trailing pane lines liveness reads (matches the supervisor)
	idleStaleSweeps  = 2                      // unchanged + not-busy sweeps before idle_unreported (matches scanStale)
)

// paneObservation is one liveness reading of a worker's tmux window. present=false
// means the window is genuinely gone (→ window_gone). present && !captured means the
// window exists but its pane could not be read this sweep (a tmux hiccup): the
// watcher skips it rather than mistake it for a gone or idle worker.
type paneObservation struct {
	present  bool
	captured bool
	pane     string
}

// Watcher runs the blocking watch loop over a Store. The seam fields reach real
// tmux / gh in production and are swapped out in tests; New wires the production
// defaults.
type Watcher struct {
	Store   *db.Store
	P       paths.Paths
	Session string
	Out     io.Writer

	// Tunables (zero ⇒ the default above).
	Since    int64         // start watermark; <0 ⇒ manager.watch_watermark (§4.2)
	Timeout  time.Duration // 0 ⇒ block forever
	Coalesce time.Duration
	Poll     time.Duration
	Stale    time.Duration // liveness last_progress_at quiet threshold (§4.4)
	PRCheck  time.Duration

	// Seams (nil ⇒ the production default).
	now            func() time.Time
	wait           func(ctx context.Context, d time.Duration) error
	capture        func(window string) paneObservation
	managerPresent func() bool
	ghAvailable    func() bool
	prState        func(prURL string) (string, error)
	isWatchProc    func(pid int) bool
	lockRetry      time.Duration // flock contention retry interval

	lastPRCheck time.Time // PR-poll rate-limit clock
}

// Result reports how the watch loop ended, for the CLI and the tests.
type Result struct {
	Fired     bool       // an actionable batch was surfaced (exit 0, re-invoke the manager)
	TimedOut  bool       // the timeout elapsed with nothing actionable (WATCH_TIMEOUT)
	SelfExit  bool       // the manager window was absent for too long (§4.5)
	Watermark int64      // the surfaced/persisted watermark (valid when Fired)
	Batch     []db.Event // the coalesced, entity-deduped batch (valid when Fired)
}

// New builds a Watcher with production seams. store and p are required; session is
// the tmux session the manager runs in.
func New(store *db.Store, p paths.Paths, session string) *Watcher {
	w := &Watcher{
		Store:    store,
		P:        p,
		Session:  session,
		Out:      os.Stdout,
		Since:    -1,
		Coalesce: defaultCoalesce,
		Poll:     defaultPoll,
		Stale:    defaultStaleHold,
		PRCheck:  defaultPRCheck,
	}
	w.now = time.Now
	w.wait = realWait
	w.lockRetry = 50 * time.Millisecond
	w.capture = func(window string) paneObservation {
		// Only a genuinely-absent window (exists==false, no read error) is gone.
		// tmux being unavailable or a list-windows hiccup is "can't observe" — skip,
		// never flag gone (§4.4 must not re-flag a worker on a transient failure).
		if !tmux.Available() {
			return paneObservation{present: true}
		}
		exists, err := tmux.WindowExistsErr(w.Session, window)
		if err != nil {
			return paneObservation{present: true}
		}
		if !exists {
			return paneObservation{}
		}
		out, err := tmux.CapturePane(w.Session, window, captureLines)
		if err != nil {
			return paneObservation{present: true}
		}
		return paneObservation{present: true, captured: true, pane: out}
	}
	w.managerPresent = func() bool {
		return tmux.Available() && tmux.WindowExists(w.Session, managerWindow)
	}
	w.ghAvailable = func() bool { _, err := exec.LookPath("gh"); return err == nil }
	w.prState = func(prURL string) (string, error) {
		out, err := exec.Command("gh", "pr", "view", prURL, "--json", "state", "-q", ".state").Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	w.isWatchProc = isWatchProcess
	return w
}

// realWait blocks for d (a short poll/coalesce interval), or until ctx is done. A
// non-positive d returns immediately (still observing cancellation). Tests inject a
// wait that advances a fake clock so the loop runs without real sleeping.
func realWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *Watcher) clock() time.Time { return w.now() }

func (w *Watcher) coalesce() time.Duration {
	if w.Coalesce <= 0 {
		return defaultCoalesce
	}
	return w.Coalesce
}

func (w *Watcher) poll() time.Duration {
	if w.Poll <= 0 {
		return defaultPoll
	}
	return w.Poll
}

func (w *Watcher) staleHold() time.Duration {
	if w.Stale < 0 {
		return 0
	}
	if w.Stale == 0 {
		return defaultStaleHold
	}
	return w.Stale
}

func (w *Watcher) prCheckEvery() time.Duration {
	if w.PRCheck <= 0 {
		return defaultPRCheck
	}
	return w.PRCheck
}

// Run acquires the watch singleton flock, then blocks on actionable events until one
// is surfaced (Fired), the timeout elapses (TimedOut), the manager window is gone for
// too long (SelfExit), or ctx is cancelled. It NEVER touches manager.awaiting_lead —
// that flag is the §4.6 backstop it merely observes; the arming command (cmdWatch)
// owns clearing it.
func (w *Watcher) Run(ctx context.Context) (Result, error) {
	lock, err := w.acquireBriefly(ctx)
	if err != nil {
		// A live watcher already owns the singleton (a slow orphan release we waited
		// out, or a genuine duplicate): this arm has no work to do, so exit quietly —
		// the holder will surface the wake. ctx cancellation propagates as an error.
		if err == errLockHeld {
			return Result{}, nil
		}
		return Result{}, err
	}
	defer releaseFlock(lock, w.P.WatchPIDFile())

	since := w.Since
	if since < 0 {
		m, _, err := w.Store.GetManager(ctx)
		if err != nil {
			return Result{}, err
		}
		since = m.WatchWatermark
	}

	start := w.clock()
	absent := 0
	checkAbsent := func() (Result, bool) {
		if w.managerPresent() {
			absent = 0
			return Result{}, false
		}
		absent++
		if absent >= mgrAbsentSweeps {
			return Result{SelfExit: true}, true
		}
		return Result{}, false
	}

	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}

		m, _, err := w.Store.GetManager(ctx)
		if err != nil {
			return Result{}, err
		}
		if m.AwaitingLead {
			// §4.6 backstop: a watcher that was not cancelled when the manager surfaced
			// a decision must keep blocking — never surface, never time out — so it
			// cannot pull the manager off the pending decision. It only self-exits if
			// the manager window itself is gone (a crash), so it can't block forever.
			if res, done := checkAbsent(); done {
				return res, nil
			}
			if err := w.wait(ctx, w.poll()); err != nil {
				return Result{}, err
			}
			continue
		}

		// Side-effecting polls: they persist external/liveness events to the DB and
		// return nothing; EventsSince below is the single source of the surfaced batch.
		if err := w.pollArmedPRs(ctx); err != nil {
			return Result{}, err
		}
		if err := w.pollLiveness(ctx); err != nil {
			return Result{}, err
		}

		rows, err := w.Store.EventsSince(ctx, since, true)
		if err != nil {
			return Result{}, err
		}
		if len(rows) > 0 {
			// Absorb a burst before surfacing, then re-read as the definitive batch.
			if err := w.wait(ctx, w.coalesce()); err != nil {
				return Result{}, err
			}
			rows, err = w.Store.EventsSince(ctx, since, true)
			if err != nil {
				return Result{}, err
			}
			batch := dedupeByEntity(rows)
			max := maxID(rows)
			w.printBatch(since, max, batch)
			// Persist the watermark BEFORE releasing the flock (the invariant in §4.2):
			// the next armed watcher must read a watermark already reflecting this batch.
			if err := w.Store.SetWatermark(ctx, max); err != nil {
				return Result{}, err
			}
			return Result{Fired: true, Watermark: max, Batch: batch}, nil
		}

		if res, done := checkAbsent(); done {
			return res, nil
		}
		if w.Timeout > 0 && w.clock().Sub(start) >= w.Timeout {
			fmt.Fprintln(w.Out, "WATCH_TIMEOUT")
			return Result{TimedOut: true}, nil
		}
		if err := w.wait(ctx, w.poll()); err != nil {
			return Result{}, err
		}
	}
}

// pollArmedPRs detects merged PRs for armed tasks (replaces supervisor.scanChecks,
// §4.4). It is side-effecting only: on MERGED it appends a pr_merged actionable
// event exactly once (durable dedup via HasEventType). Requires gh; degrades
// silently when absent, exactly as the supervisor did.
func (w *Watcher) pollArmedPRs(ctx context.Context) error {
	if !w.ghAvailable() {
		return nil
	}
	now := w.clock()
	if !w.lastPRCheck.IsZero() && now.Sub(w.lastPRCheck) < w.prCheckEvery() {
		return nil
	}
	w.lastPRCheck = now
	tasks, err := w.Store.ListTasks(ctx, db.TaskFilter{})
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.PR == "" {
			continue
		}
		merged, err := w.Store.HasEventType(ctx, t.ID, db.EventPRMerged)
		if err != nil {
			return err
		}
		if merged {
			continue
		}
		state, err := w.prState(t.PR)
		if err != nil {
			continue // a single failed/absent PR read never aborts the sweep
		}
		if state != "MERGED" {
			continue
		}
		if _, err := w.Store.AppendEvent(ctx, db.Event{
			EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventPRMerged,
			Actor: db.ActorSystem, Actionable: true, Payload: t.PR,
		}); err != nil {
			return err
		}
	}
	return nil
}

// pollLiveness is the stale/gone safety net (replaces supervisor.scanStale, §4.4).
// It is scoped to status='active', non-cc tasks whose last_progress_at is stale and
// which are not already surfaced (no open unresolved actionable event). A gone window
// emits window_gone; an unchanged, not-busy pane across idleStaleSweeps sweeps emits
// idle_unreported. The per-task sweep count is persisted (SetLiveness) so it survives
// across short-lived watch invocations; a pane change resets it.
func (w *Watcher) pollLiveness(ctx context.Context) error {
	tasks, err := w.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusActive},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return err
	}
	now := w.clock()
	hold := w.staleHold()
	for _, t := range tasks {
		if t.Window == "" || t.Window == managerWindow {
			continue
		}
		// Quiet-long-enough gate: a worker that reported progress recently is not a
		// stall candidate (nil last_progress_at = never reported = eligible).
		if hold > 0 && t.LastProgressAt != nil && now.Sub(*t.LastProgressAt) < hold {
			continue
		}
		// Already-surfaced gate: a task already in needs_input/blocked is out of scope
		// (not 'active'); one carrying an unresolved actionable event (a prior
		// window_gone/idle_unreported, or a pending pr_merged) must not be re-flagged.
		surfaced, err := w.Store.HasActionableEventForTask(ctx, t.ID, t.LastProgressAt)
		if err != nil {
			return err
		}
		if surfaced {
			continue
		}

		obs := w.capture(t.Window)
		if !obs.present {
			if _, err := w.Store.AppendEvent(ctx, db.Event{
				EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventWindowGone,
				Actor: db.ActorSystem, Actionable: true, Payload: t.Window,
			}); err != nil {
				return err
			}
			continue
		}
		if !obs.captured {
			continue // present but unreadable this sweep — leave the count untouched
		}
		if livestate.Busy(obs.pane) {
			// Mid-turn: reset the idle bookkeeping so a busy worker never goes stale.
			if err := w.setLiveness(ctx, t, hashPane(obs.pane), 0); err != nil {
				return err
			}
			continue
		}
		h := hashPane(obs.pane)
		if t.LastPaneHash == h {
			sweeps := t.IdleSweeps + 1
			// `>=`, not `==`: the already-surfaced gate above masks re-fires within one
			// idle episode (the idle_unreported event is newer than last_progress_at),
			// so this only re-fires after that exclusion clears — e.g. an out-of-band
			// progress update advanced last_progress_at without changing the pane,
			// leaving idle_sweeps stranded past the threshold. An exact-equality test
			// would leave such a worker permanently un-flaggable until its pane changed.
			if sweeps >= idleStaleSweeps {
				if _, err := w.Store.AppendEvent(ctx, db.Event{
					EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventIdleUnreported,
					Actor: db.ActorSystem, Actionable: true, Payload: t.Window,
				}); err != nil {
					return err
				}
			}
			if err := w.setLiveness(ctx, t, h, sweeps); err != nil {
				return err
			}
		} else {
			if err := w.setLiveness(ctx, t, h, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// setLiveness persists the pane hash + idle sweep count, avoiding a needless write
// when nothing changed (the hot path: a busy worker re-confirmed each sweep).
func (w *Watcher) setLiveness(ctx context.Context, t db.Task, paneHash string, sweeps int) error {
	if t.LastPaneHash == paneHash && t.IdleSweeps == sweeps {
		return nil
	}
	return w.Store.SetLiveness(ctx, t.ID, paneHash, sweeps)
}

// dedupeByEntity collapses the actionable rows to one per entity_id, keeping the row
// with the maximum events.id (the latest transition wins, §4.3) so a task that went
// blocked → active → done surfaces a single → done line, never a stale → blocked
// alongside it. The result is ordered by id ascending for a stable batch.
func dedupeByEntity(rows []db.Event) []db.Event {
	best := make(map[string]db.Event, len(rows))
	for _, e := range rows {
		if cur, ok := best[e.EntityID]; !ok || e.ID > cur.ID {
			best[e.EntityID] = e
		}
	}
	out := make([]db.Event, 0, len(best))
	for _, e := range best {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// maxID returns the highest events.id among the rows (0 for an empty slice). The
// watermark is the max over ALL returned actionable rows, not just the deduped batch
// (§4.2), so a superseded earlier transition still advances the consumed watermark.
func maxID(rows []db.Event) int64 {
	var max int64
	for _, e := range rows {
		if e.ID > max {
			max = e.ID
		}
	}
	return max
}

// printBatch renders the coalesced batch in the §4.3 format, ending with the
// machine-readable WATCH_WATERMARK line the manager's harness keys on.
func (w *Watcher) printBatch(since, max int64, batch []db.Event) {
	fmt.Fprintf(w.Out, "ttorch watch: %d actionable update(s) since #%d (now #%d)\n", len(batch), since, max)
	for _, e := range batch {
		fmt.Fprintln(w.Out, "  "+formatEventLine(e))
	}
	fmt.Fprintln(w.Out, "next: ttorch tasks --status done,blocked,needs_input ; then land / answer / dispatch")
	fmt.Fprintf(w.Out, "WATCH_WATERMARK=%d\n", max)
}

// formatEventLine renders one surfaced event (§4.3). Status transitions show
// from → to plus the owner or the worker's message; the watcher-generated external
// events get their own one-liners.
func formatEventLine(e db.Event) string {
	id := " (#" + strconv.FormatInt(e.ID, 10) + ")"
	switch e.Type {
	case db.EventPRMerged:
		return fmt.Sprintf("pr-merged             task=%-18s %s%s", e.EntityID, e.Payload, id)
	case db.EventWindowGone:
		return fmt.Sprintf("window-gone           task=%-18s window=%s%s", e.EntityID, e.Payload, id)
	case db.EventIdleUnreported:
		return fmt.Sprintf("idle-unreported       task=%-18s window=%s%s", e.EntityID, e.Payload, id)
	default:
		detail := ""
		if e.Payload != "" {
			detail = fmt.Sprintf("%q", e.Payload)
		}
		return fmt.Sprintf("task=%-18s %s → %-12s %s%s",
			e.EntityID, derefStatus(e.FromStatus), derefStatus(e.ToStatus), detail, id)
	}
}

func derefStatus(s *string) string {
	if s == nil {
		return "?"
	}
	return *s
}
