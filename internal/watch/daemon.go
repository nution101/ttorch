package watch

// The scheduler's watch loop (Daemon) is `ttorch watch` made always-on. A manually armed
// watcher only listens while the manager session keeps it armed, and the manager has to
// re-arm it after every turn; when the manager stalls or forgets, worker completions,
// needs-input reports and gate escalations sit unnoticed. The Daemon runs inside the
// scheduler process for as long as that process runs, with no LLM involvement:
//
//   - Detection reuses the Watcher's own sweeps (pollArmedPRs, pollLiveness), so a merged
//     PR, a gone window or an idle-unreported worker is recorded exactly as an armed
//     watcher would record it.
//   - The inbox is the existing one: every actionable event above
//     manager.watch_watermark. The Daemon never advances the watermark; only a consumer
//     (`ttorch inbox`, or an armed `ttorch watch` firing) does. An update recorded while
//     the manager was busy, while no manager window existed, or before a scheduler
//     restart is therefore still unread afterwards.
//   - Waking: when updates are unread, the manager is not awaiting the lead, and the
//     manager pane sits at an empty prompt, the Daemon types one fixed wake line into the
//     manager window (wireManagerWake). It never types into a busy pane, a pane where the
//     lead has started typing, or a pane that has fallen back to a shell. At most one wake
//     is outstanding: after a wake, further updates add nothing until the manager consumes
//     its inbox, and an unanswered wake is repeated only on the slow Rewake cadence. Each
//     wake is recorded as a non-actionable manager_woken event carrying the highest unread
//     id it announced, which is what makes the coalescing survive a restart.
//
// Singleton: each sweep takes the watch flock without waiting. If an armed `ttorch watch`
// holds it, the Daemon stands down for that sweep and neither records nor wakes, because
// the armed watcher will surface the batch itself. The Daemon releases the flock at the end
// of every sweep, so a manual arm that collides with a sweep waits for it rather than being
// refused: the Daemon records a token that never matches a manager pane and a process that
// is not a `ttorch watch`, so acquire classifies it as a holder it cannot reap and retries
// for resetAcquireGrace. A sweep is a few DB reads plus one pane capture per active worker,
// and the rate-limited PR poll, so it finishes well inside that window.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/backend"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/tmux"
)

// EventManagerWoken records one wake line typed into the manager window. It is
// non-actionable (it must never itself look like an update), entity_type=manager, and its
// payload is the highest unread actionable events.id the wake announced.
const EventManagerWoken = "manager_woken"

// daemonToken is the instance token the Daemon records in the watch pid file while it holds
// the flock for a sweep. It can never equal a manager pane token ("pane:<pid>:<start>"), so
// a manual arm that collides with a sweep treats the Daemon as a foreign holder and waits.
const daemonToken = "scheduler-watch"

const (
	defaultDaemonPoll = 2 * time.Second  // sweep cadence
	defaultRewake     = 3 * time.Minute  // repeat an unanswered wake no faster than this
	defaultSendRetry  = 30 * time.Second // retry spacing after a send that failed (e.g. copy-mode)
)

// Daemon is the scheduler-owned watch loop. NewDaemon wires the production seams; tests
// replace them.
type Daemon struct {
	Store   *db.Store
	P       paths.Paths
	Session string
	Log     io.Writer // the scheduler's log, never the manager pane; nil silences it

	Poll      time.Duration // sweep cadence (0 ⇒ defaultDaemonPoll)
	Rewake    time.Duration // cadence for repeating an unanswered wake (0 ⇒ defaultRewake)
	SendRetry time.Duration // spacing after a failed send (0 ⇒ defaultSendRetry)

	// w supplies detection (pollArmedPRs, pollLiveness), the manager-pane capture, and the
	// clock/wait seams, so the Daemon and an armed watcher share one implementation.
	w *Watcher

	// Seams (wired by wireManagerWake in production).
	managerCommand func() string // the manager pane's foreground command (tmux pane_current_command)
	sendWake       func() error  // type the fixed wake line into the manager window

	retryAt time.Time // earliest next send after a failed one; in memory, a restart just retries
}

// DaemonTick reports what one sweep did, for the scheduler log and the tests.
type DaemonTick struct {
	Standby bool   // an armed `ttorch watch` holds the singleton; this sweep did nothing
	Unread  int    // actionable updates above the watermark after the sweep
	Woke    bool   // a wake line was typed into the manager window
	Reason  string // why no wake was typed (empty when Woke)
}

// NewDaemon builds the scheduler's watch loop with production seams. be is the session
// backend and session the session in it that the manager runs in, as for New. log receives
// its diagnostic lines (the scheduler log).
func NewDaemon(store *db.Store, p paths.Paths, be backend.Backend, session string, log io.Writer) *Daemon {
	d := &Daemon{
		Store:     store,
		P:         p,
		Session:   session,
		Log:       log,
		Poll:      defaultDaemonPoll,
		Rewake:    defaultRewake,
		SendRetry: defaultSendRetry,
	}
	d.w = New(store, p, be, session)
	d.w.Out = io.Discard // the Daemon never prints a watch batch; `ttorch inbox` does
	wireManagerWake(d, session)
	return d
}

// wireManagerWake installs the production seams that reach the manager window. It holds the
// one send into the manager window that the watch loop makes, and the payload is a fixed
// string LITERAL: the source-scan invariant orchestrator.TestNoInjectionIntoManagerSession
// allow-lists exactly this file, this top-level function and this literal, so no event
// payload or other content can ever be typed into the manager. The line is also inert if it
// ever reaches a shell (no backticks, $, quotes, pipes or separators; its first word is not
// a command), which backs up the shell check in Tick.
func wireManagerWake(d *Daemon, session string) {
	d.managerCommand = func() string { return tmux.PaneCurrentCommand(session, managerWindow) }
	d.sendWake = func() error {
		return tmux.SendLine(session, managerWindow, "Scheduler wake: unread updates are waiting. Run ttorch inbox and act on them.")
	}
}

func (d *Daemon) poll() time.Duration {
	if d.Poll <= 0 {
		return defaultDaemonPoll
	}
	return d.Poll
}

func (d *Daemon) rewake() time.Duration {
	if d.Rewake <= 0 {
		return defaultRewake
	}
	return d.Rewake
}

func (d *Daemon) sendRetry() time.Duration {
	if d.SendRetry <= 0 {
		return defaultSendRetry
	}
	return d.SendRetry
}

// Run sweeps every Poll until ctx is cancelled. A sweep error is logged and the loop goes
// on, so a DB hiccup never stops the watching; only cancellation ends it.
func (d *Daemon) Run(ctx context.Context) error {
	for {
		if _, err := d.Tick(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.logf("watch: sweep error: %v", err)
		}
		if err := d.w.wait(ctx, d.poll()); err != nil {
			return err
		}
	}
}

// Tick runs one sweep: take the watch flock (or stand down for an armed watcher), record
// what the Watcher's sweeps detect, and wake the manager if the conditions in the file
// header hold. The flock is held through the wake so an armed watcher cannot start
// reporting the same updates between the decision and the send.
func (d *Daemon) Tick(ctx context.Context) (DaemonTick, error) {
	path := d.P.WatchPIDFile()
	lock, err := acquireFlock(path, daemonToken)
	if err == errLockHeld {
		return DaemonTick{Standby: true, Reason: "an armed ttorch watch holds the singleton and will report"}, nil
	}
	if err != nil {
		return DaemonTick{}, err
	}
	defer releaseFlock(lock, path)

	if err := d.w.pollArmedPRs(ctx); err != nil {
		return DaemonTick{}, err
	}
	if err := d.w.pollLiveness(ctx); err != nil {
		return DaemonTick{}, err
	}

	m, _, err := d.Store.GetManager(ctx)
	if err != nil {
		return DaemonTick{}, err
	}
	unread, err := d.Store.EventsSince(ctx, m.WatchWatermark, true)
	if err != nil {
		return DaemonTick{}, err
	}
	res := DaemonTick{Unread: len(unread)}
	if len(unread) == 0 {
		res.Reason = "inbox empty"
		return res, nil
	}
	if m.AwaitingLead {
		res.Reason = "manager is awaiting the lead; recorded only"
		return res, nil
	}
	if reason := d.managerNotWakeable(); reason != "" {
		res.Reason = reason
		return res, nil
	}

	now := d.w.clock()
	last, ok, err := d.Store.LatestEvent(ctx, db.EntityTypeManager, managerEntityID, EventManagerWoken)
	if err != nil {
		return DaemonTick{}, err
	}
	if ok && announcedID(last) > m.WatchWatermark && now.Sub(last.TS) < d.rewake() {
		res.Reason = "a wake is already pending"
		return res, nil
	}
	if now.Before(d.retryAt) {
		res.Reason = "retrying a failed wake later"
		return res, nil
	}

	top := maxID(unread)
	if err := d.sendWake(); err != nil {
		d.retryAt = now.Add(d.sendRetry())
		d.logf("watch: could not wake the manager (retrying in %s): %v", d.sendRetry(), err)
		res.Reason = "wake send failed"
		return res, nil
	}
	res.Woke = true
	if _, err := d.Store.AppendEvent(ctx, db.Event{
		TS:         now,
		EntityType: db.EntityTypeManager,
		EntityID:   managerEntityID,
		Type:       EventManagerWoken,
		Actor:      db.ActorSystem,
		Actionable: false,
		Payload:    strconv.FormatInt(top, 10),
	}); err != nil {
		// The wake landed but was not recorded, so the next sweep would treat it as never
		// sent. Hold further sends for a full Rewake instead of waking again at once.
		d.retryAt = now.Add(d.rewake())
		return res, err
	}
	d.logf("watch: woke the manager: %d unread update(s) up to #%d", len(unread), top)
	return res, nil
}

// managerNotWakeable returns why the manager window must not be typed into right now, or ""
// when it may be: the window exists and was read, its foreground process is not a shell,
// and the pane sits at an empty Claude Code prompt.
func (d *Daemon) managerNotWakeable() string {
	obs := d.w.capture(managerWindow)
	if !obs.present {
		return "no manager window; recorded only"
	}
	if !obs.captured {
		return "manager pane unreadable"
	}
	if cmd := d.managerCommand(); unsafeToType(cmd) {
		return fmt.Sprintf("manager pane is running %q, not the harness", cmd)
	}
	if !managerAtEmptyPrompt(obs.pane) {
		return "manager is busy or the prompt is not empty"
	}
	return ""
}

// announcedID parses the highest unread id a manager_woken event announced (0 when the
// payload is missing or garbled, which reads as "no pending wake").
func announcedID(e db.Event) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(e.Payload), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// unsafeToType reports whether the manager pane's foreground command rules out typing into
// it. A shell there means the harness exited and anything typed would be executed; an empty
// answer means tmux could not say, and the wake fails closed.
func unsafeToType(cmd string) bool {
	switch strings.TrimPrefix(strings.TrimSpace(cmd), "-") {
	case "", "sh", "bash", "zsh", "fish", "dash", "ksh", "tcsh", "csh", "nu":
		return true
	}
	return false
}

// spinnerLine matches the harness's in-progress status line, e.g.
// "· Pondering… (6m 13s · ↓ 19.0k tokens)" or "✢ Cogitating… (12s · esc to interrupt)": a
// glyph, a capitalized verb ending in an ellipsis, then an opening parenthesis. The verb
// changes from turn to turn, so livestate.Busy's fixed word list does not catch it. The
// finished form ("✻ Baked for 1m 18s") has no ellipsis and does not match.
var spinnerLine = regexp.MustCompile(`^\s*[^\p{L}\p{N}\s]\s+\p{Lu}[\p{L}'-]*(?:…|\.\.\.)\s*\(`)

// promptBorders is the input box's border cutset, as livestate strips it.
const promptBorders = " \t│┃┆┇┊┋╎╏║|"

// managerAtEmptyPrompt reports whether a manager pane capture shows the harness idle at an
// EMPTY input prompt, the only state the wake may type into. It starts from livestate.Busy
// and is stricter than livestate.Idle in two ways that matter for a pane the lead also types
// into:
//
//   - it rejects the harness's gerund spinner line, which the current harness shows while
//     busy and which livestate.Busy's word list misses; and
//   - it requires the input line to be empty (or showing the "Try …" placeholder). A
//     prompt holding other text is the lead part-way through a message, and typing the
//     wake plus Enter there would submit the lead's draft.
//
// It accepts both prompt carets: ">" (as livestate.Idle does) and "❯", which the current
// harness renders. The input line is found bottom-up, so a quoted "> " line in the history
// above the input box is never mistaken for it. Anything it cannot place reads as not idle.
func managerAtEmptyPrompt(pane string) bool {
	if livestate.Busy(pane) {
		return false
	}
	lines := strings.Split(pane, "\n")
	for _, l := range lines {
		if spinnerLine.MatchString(l) {
			return false
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(strings.Trim(lines[i], promptBorders))
		rest, ok := cutPromptCaret(s)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		return rest == "" || strings.HasPrefix(rest, `Try "`)
	}
	return false
}

// cutPromptCaret strips a leading prompt caret from a border-stripped line.
func cutPromptCaret(s string) (string, bool) {
	for _, caret := range []string{">", "❯"} {
		if s == caret {
			return "", true
		}
		if rest, ok := strings.CutPrefix(s, caret+" "); ok {
			return rest, true
		}
	}
	return "", false
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, "scheduler: "+format+"\n", args...)
}
