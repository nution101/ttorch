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
	"os/exec"
	"path/filepath"
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

	// After typing the wake, the loop re-reads the pane before pressing Enter: first after
	// wakeSettle (the pause SendLine used before its Enter), then every wakeRecheck while the
	// input still shows only part of the line, for wakeConfirmReads reads in all.
	wakeSettle       = 300 * time.Millisecond
	wakeRecheck      = 200 * time.Millisecond
	wakeConfirmReads = 5
)

// wakeLine is the text wireManagerWake types, for comparing against what the pane shows
// before Enter is pressed. The send itself must use the literal (the injection invariant can
// only prove a literal's value); TestWakeLiteralMatchesWakeLine keeps the two identical.
const wakeLine = "Automated notice from the ttorch scheduler, not the lead: unread worker updates, run ttorch inbox"

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
	managerForeground func() string // argv of the manager pane's foreground process group leader ("" if unknown)
	typeWake          func() error  // type the fixed wake line into the manager input, without Enter
	pressEnter        func() error  // submit it, once the pane shows exactly wakeLine

	retryAt time.Time // earliest next send after a failed one; in memory, a restart just retries

	// inStandby is true from the first sweep that found the watch lock held by someone else
	// until the next sweep that takes it, so each standby episode is logged once.
	inStandby bool
}

// DaemonTick reports what one sweep did, for the scheduler log and the tests.
type DaemonTick struct {
	Standby   bool   // an armed `ttorch watch` holds the singleton; this sweep did nothing
	HolderPID int    // with Standby: the holder's recorded pid (0 if the pid file could not be read)
	Unread    int    // actionable updates above the watermark after the sweep
	Woke      bool   // a wake line was typed into the manager window
	Reason    string // why no wake was typed (empty when Woke)
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
// payload or other content can ever be typed into the manager. The line lands in the input
// the lead types into, so it says it is automated and not from the lead, and it names only
// the command to run: it gives no instruction a reader could mistake for the lead's. It is
// also inert if it ever reaches a shell (no backticks, $, quotes, pipes or separators; its
// first word is not a command), which backs up the foreground check in Tick.
//
// Typing and submitting are separate sends so Tick can re-read the pane in between: Enter is
// pressed only when the input holds exactly the wake line. Enter is the only key the loop ever
// sends to the manager, and the allow-list pins that too.
func wireManagerWake(d *Daemon, session string) {
	d.managerForeground = func() string { return foregroundLeader(tmux.PanePID(session, managerWindow)) }
	d.typeWake = func() error {
		return tmux.TypeLine(session, managerWindow, "Automated notice from the ttorch scheduler, not the lead: unread worker updates, run ttorch inbox")
	}
	d.pressEnter = func() error {
		return tmux.SendKey(session, managerWindow, "Enter")
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
		rec, _ := readWatchRecord(path)
		if !d.inStandby {
			d.inStandby = true
			holder := "another process"
			if rec.pid > 0 {
				holder = "pid " + strconv.Itoa(rec.pid)
			}
			d.logf("watch: standing by: the watch lock is held by %s, and that watcher reports updates instead", holder)
		}
		return DaemonTick{Standby: true, HolderPID: rec.pid, Reason: "an armed ttorch watch holds the singleton and will report"}, nil
	}
	if err != nil {
		return DaemonTick{}, err
	}
	defer releaseFlock(lock, path)
	if d.inStandby {
		d.inStandby = false
		d.logf("watch: resumed: the watch lock is free again")
	}

	if err := d.w.pollArmedPRs(ctx); err != nil {
		return DaemonTick{}, err
	}
	if err := d.w.pollLiveness(ctx); err != nil {
		return DaemonTick{}, err
	}

	// Two separate reads: an inbox read landing between them can cost one superfluous wake (the manager finds an empty inbox). Accepted.
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
	if err := d.typeWake(); err != nil {
		d.retryAt = now.Add(d.sendRetry())
		d.logf("watch: could not wake the manager (retrying in %s): %v", d.sendRetry(), err)
		res.Reason = "wake send failed"
		return res, nil
	}
	// The lead may have started typing between the idle check and the keys landing. Enter is
	// pressed only if the input now holds exactly the wake line. Otherwise the typed text is
	// left unsubmitted: pressing Enter could send the lead's keystrokes, and clearing the line
	// could erase them. A prompt holding text is not idle, so no further wake is typed until
	// the line is cleared.
	if why := d.confirmTypedWake(ctx); why != "" {
		d.retryAt = now.Add(d.sendRetry())
		d.logf("watch: typed the wake but did not submit it: %s", why)
		res.Reason = "wake not submitted: " + why
		return res, nil
	}
	if err := d.pressEnter(); err != nil {
		d.retryAt = now.Add(d.sendRetry())
		d.logf("watch: typed the wake but could not submit it (retrying in %s): %v", d.sendRetry(), err)
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

// StandbyHolder reports whether a hand-armed `ttorch watch` holds the watch singleton, which
// is when the scheduler's watch loop stands by, and that watcher's pid. It only reads the pid
// file and probes the recorded pid; it never takes the flock, so `ttorch status` can call it
// without colliding with a sweep or an arm. A record left by the loop itself, a dead pid, or
// a pid that is no longer a `ttorch watch` (reused) all report false.
func StandbyHolder(p paths.Paths) (int, bool) {
	return standbyHolder(p.WatchPIDFile(), processAlive, isWatchProcess)
}

func standbyHolder(path string, alive, isWatch func(int) bool) (int, bool) {
	rec, ok := readWatchRecord(path)
	if !ok || rec.token == daemonToken || !alive(rec.pid) || !isWatch(rec.pid) {
		return 0, false
	}
	return rec.pid, true
}

// managerNotWakeable returns why the manager window must not be typed into right now, or ""
// when it may be: the window exists and was read, its foreground process is the harness,
// and the pane sits at an empty Claude Code prompt.
func (d *Daemon) managerNotWakeable() string {
	obs := d.w.capture(managerWindow)
	if !obs.present {
		return "no manager window; recorded only"
	}
	if !obs.captured {
		return "manager pane unreadable"
	}
	if fg := d.managerForeground(); !isHarnessCommand(fg) {
		return fmt.Sprintf("manager pane foreground is %q, not the harness", firstField(fg))
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

// isHarnessCommand reports whether args, the command line of the manager pane's foreground
// process group leader, is the Claude Code harness. It is an allowlist: the program is
// `claude` (by name, or the versioned native binary under .../claude/versions/), or `node` /
// `bun` whose script is `claude` or lives in the @anthropic-ai/claude-code package. Anything
// else refuses, including a shell (the harness exited, and typed text would be executed),
// ssh, sudo, an editor, or an empty answer because the leader could not be read.
func isHarnessCommand(args string) bool {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return false
	}
	if isClaudeProgram(fields[0]) {
		return true
	}
	switch filepath.Base(fields[0]) {
	case "node", "bun":
		for _, a := range fields[1:] {
			if strings.HasPrefix(a, "-") {
				continue // runtime flags before the script
			}
			return isClaudeProgram(a) || strings.Contains(filepath.ToSlash(a), "/@anthropic-ai/claude-code/")
		}
	}
	return false
}

func isClaudeProgram(path string) bool {
	return filepath.Base(path) == "claude" || strings.Contains(filepath.ToSlash(path), "/claude/versions/")
}

// foregroundLeader returns the command line of the process group leader in the foreground of
// the terminal that panePID (the manager pane's first process, normally the shell that
// launched the harness) is attached to, or "" when any step cannot be read. tmux's
// pane_current_command gives only a process name (the native harness shows up as its version
// number), so the allowlist needs the leader's argv. The foreground group also holds the
// harness's own children (MCP servers), which is why only the leader is read.
func foregroundLeader(panePID int) string {
	if panePID <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "tpgid=", "-p", strconv.Itoa(panePID)).Output()
	if err != nil {
		return ""
	}
	tpgid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || tpgid <= 0 {
		return ""
	}
	out, err = exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(tpgid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// firstField is the program part of a command line, for log lines that must not echo the
// harness's full argument list.
func firstField(args string) string {
	if f := strings.Fields(args); len(f) > 0 {
		return filepath.Base(f[0])
	}
	return ""
}

// spinnerLine matches the harness's in-progress status line, e.g.
// "· Pondering… (6m 13s · ↓ 19.0k tokens)" or "✢ Cogitating… (12s · esc to interrupt)": a
// glyph, a capitalized verb ending in an ellipsis, then an opening parenthesis. The verb
// changes from turn to turn, so livestate.Busy's fixed word list does not catch it. The
// finished form ("✻ Baked for 1m 18s") has no ellipsis and does not match.
var spinnerLine = regexp.MustCompile(`^\s*[^\p{L}\p{N}\s]\s+\p{Lu}[\p{L}'-]*(?:…|\.\.\.)\s*\(`)

// promptBorders is the input box's border cutset, as livestate strips it.
const promptBorders = " \t│┃┆┇┊┋╎╏║|"

// placeholderInput matches the harness's empty-input placeholder exactly: Try "<suggestion>"
// and nothing else. A line that merely starts that way is the lead typing.
var placeholderInput = regexp.MustCompile(`^Try "[^"]+"$`)

// confirmTypedWake re-reads the manager pane after the wake was typed and returns "" when the
// input holds exactly wakeLine and the harness is still idle, or why it does not. A strict
// prefix of the line means the harness is still rendering the keys, so it reads again, up to
// wakeConfirmReads times.
func (d *Daemon) confirmTypedWake(ctx context.Context) string {
	pause := wakeSettle
	for i := 0; i < wakeConfirmReads; i++ {
		if err := d.w.wait(ctx, pause); err != nil {
			return "cancelled"
		}
		pause = wakeRecheck
		obs := d.w.capture(managerWindow)
		if !obs.present || !obs.captured {
			return "manager pane unreadable after typing"
		}
		if harnessBusy(obs.pane) {
			return "the manager became busy"
		}
		input, ok := promptInput(obs.pane)
		switch {
		case !ok:
			return "no input prompt after typing"
		case input == wakeLine:
			return ""
		case !strings.HasPrefix(wakeLine, input):
			return "the input holds other text"
		}
	}
	return "the typed wake never fully rendered"
}

// harnessBusy reports a pane mid-turn: a livestate.Busy marker or the gerund spinner line.
func harnessBusy(pane string) bool {
	if livestate.Busy(pane) {
		return true
	}
	for _, l := range strings.Split(pane, "\n") {
		if spinnerLine.MatchString(l) {
			return true
		}
	}
	return false
}

// promptInput returns the text in the harness's input box, whitespace-normalized, and whether
// an input prompt was found. The prompt is the bottom-most line opening with a caret (">" or
// "❯"), so a quoted "> " line in the history above is never mistaken for it. The input runs
// from that line down to the box's closing border, or the end of the capture: a draft can
// span several lines, and a long line wraps onto the next, so every line in that region
// counts. Joining on whitespace lets a wake line that wrapped at a space compare equal to the
// literal.
func promptInput(pane string) (string, bool) {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := cutPromptCaret(strings.TrimSpace(strings.Trim(lines[i], promptBorders)))
		if !ok {
			continue
		}
		parts := []string{rest}
		for _, l := range lines[i+1:] {
			s := strings.TrimSpace(strings.Trim(l, promptBorders))
			if isBorderLine(s) {
				break
			}
			parts = append(parts, s)
		}
		return strings.Join(strings.Fields(strings.Join(parts, " ")), " "), true
	}
	return "", false
}

// isBorderLine reports a line made only of box-drawing glyphs: the rule or box edge that
// closes the input area. A blank line is not a border.
func isBorderLine(s string) bool {
	return s != "" && strings.Trim(s, "─━┄┅┈┉╌╍╭╮╰╯╴╵╶╷ ") == ""
}

// managerAtEmptyPrompt reports whether a manager pane capture shows the harness idle at an
// EMPTY input prompt, the only state the wake may type into. It starts from livestate.Busy
// and is stricter than livestate.Idle in ways that matter for a pane the lead also types
// into:
//
//   - it rejects the harness's gerund spinner line, which the current harness shows while
//     busy and which livestate.Busy's word list misses;
//   - it requires the whole input area to be empty, or to show exactly the harness's
//     placeholder. Text on the caret line, or on any line below it inside the input box, is
//     the lead part-way through a message, and typing the wake plus Enter there would submit
//     the lead's draft.
//
// It accepts both prompt carets: ">" (as livestate.Idle does) and "❯", which the current
// harness renders. Anything it cannot place reads as not idle.
func managerAtEmptyPrompt(pane string) bool {
	if harnessBusy(pane) {
		return false
	}
	input, ok := promptInput(pane)
	return ok && (input == "" || placeholderInput.MatchString(input))
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
