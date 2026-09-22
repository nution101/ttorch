// Package tmux drives the tmux binary via os/exec. Each worker (and the manager)
// lives in its own window of a shared session so the lead can watch or intervene.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SessionName is the tmux session ttorch uses (override with TTORCH_TMUX_SESSION).
func SessionName() string {
	if s := os.Getenv("TTORCH_TMUX_SESSION"); s != "" {
		return s
	}
	return "ttorch"
}

// TitleFormat is the set-titles-string ttorch applies to the sessions it creates
// so the terminal tab shows a window's friendly display label (the @ttorch_label
// window option set by LabelWindow), falling back to the window's tmux name when
// no label is set. Without set-titles the terminal shows its own fallback (e.g.
// iTerm's "tmux N"). The view sessions in package termtab set the same string.
const TitleFormat = "#{?#{@ttorch_label},#{@ttorch_label},#W}"

// Available reports whether tmux is installed.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// InsideTmux reports whether the current process is running inside tmux.
func InsideTmux() bool { return os.Getenv("TMUX") != "" }

func bin() (string, error) {
	p, err := exec.LookPath("tmux")
	if err != nil {
		return "", errors.New("tmux not found (run 'ttorch doctor')")
	}
	return p, nil
}

// runTimeout bounds every tmux control command below. It is a var so a test can
// shorten it; nothing changes it at runtime.
//
// A tmux command can block forever rather than fail. The reachable case is a pane
// left in copy-mode: a `send-keys` at that pane never returns, so a steer through
// SendLine hangs, and with it whatever called it — ttorch send, the scheduler's
// stall recovery, the watcher's liveness read, the gate, a spawn. Losing the
// control channel to a running agent is the worst outcome available here, and an
// error the caller can report is strictly better than a goroutine parked forever.
//
// Two measured properties of that wedge, because both mislead someone debugging it:
//
// It is send-keys-specific, not a stuck server. With a send sitting blocked at a
// wedged pane, display-message, list-windows, capture-pane, has-session,
// new-session and kill-session all answered in 0.05s on the same server, and the
// send was still blocked afterwards. So a healthy `tmux list-windows` says nothing
// about whether a steer can land.
//
// Whether it blocks depends on the literal being sent. Re-wedging the pane before
// each attempt, "continue" (what the scheduler and watcher send), "go ahead",
// "TEST" and a bare "T" all blocked past 8s, while "RAW_WEDGE" and a bare "S"
// returned rc=0 in 0.05s. The exact rule was not pinned down; the point is that one
// literal returning does not mean the pane is steerable.
//
// 30s is chosen against measurement, not taste: the slowest legitimate call is a
// capture-pane over a full scrollback, which takes 0.28s for 200,000 lines
// (208MB) on this hardware, and the control commands are ~35ms. 30s is roughly
// 100x that ceiling, so it cannot fire on a merely slow machine, while still
// bounding a scheduler tick at 30s instead of forever.
var runTimeout = 30 * time.Second

// ErrTimeout marks a tmux command that ran past runTimeout, so a caller can tell
// "tmux did not answer" from "tmux answered no". Wrapped into the error run()
// returns, so test with errors.Is.
//
// It matters because the two are not interchangeable anywhere a negative answer
// causes an action. WindowExists is the case: collapsing a timeout to "the window
// is gone" would let Resume rebuild a second agent into a worktree that already
// has one.
var ErrTimeout = errors.New("tmux did not answer in time")

// runWaitDelay bounds the wait AFTER the deadline kills the tmux client. Killing
// the client does not close the output pipe if it left a child holding the write
// end, and CombinedOutput reads until EOF — so without this the deadline would be
// no deadline at all. It only ever applies on the timeout path.
var runWaitDelay = 2 * time.Second

func run(args ...string) (string, error) {
	b, err := bin()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b, args...)
	cmd.WaitDelay = runWaitDelay
	out, err := cmd.CombinedOutput()
	s := strings.TrimRight(string(out), "\n")
	if ctx.Err() == context.DeadlineExceeded {
		return s, fmt.Errorf("tmux %s: no answer after %s, gave up; a pane left in copy-mode does this, so press Escape in the worker's pane (or 'tmux send-keys -X cancel') and retry: %w", strings.Join(args, " "), runTimeout, ErrTimeout)
	}
	if err != nil {
		return s, fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

func target(session, window string) string { return session + ":" + window }

// HasSession reports whether the named session exists.
func HasSession(session string) bool {
	_, err := run("has-session", "-t", session)
	return err == nil
}

// applySessionOptions applies the per-session options ttorch needs on a session it
// owns. set-option is per-session, so these are (re)applied to every session ttorch
// attaches (this shared session, and the view sessions in package termtab).
// Best-effort: a failure never blocks a spawn.
//
// It first pins destroy-unattached off: this shared session holds every worker's
// window, so it must NEVER self-destruct when no client is attached. A client
// disconnect (e.g. the lead's remote tmux client dropping) leaves the session
// momentarily unattached, and without this pin a global "destroy-unattached on"
// inherited from the user's tmux config would tear the session down, exit the
// server, and kill every live worker along with its uncommitted work. Pinning off
// at the session level overrides any such global default. It then turns on
// terminal-title reporting and points it at TitleFormat so the terminal tab shows a
// window's friendly display label (the @ttorch_label window option set by
// LabelWindow), falling back to the window's tmux name.
func applySessionOptions(session string) {
	_, _ = run("set-option", "-t", session, "destroy-unattached", "off")
	_, _ = run("set-option", "-t", session, "set-titles", "on")
	_, _ = run("set-option", "-t", session, "set-titles-string", TitleFormat)
}

// EnsureSession creates the session detached if it does not exist. It (re)applies
// ttorch's session options each call so a pre-existing session also pins
// destroy-unattached off and reports titles.
func EnsureSession(session string) error {
	if HasSession(session) {
		applySessionOptions(session)
		return nil
	}
	if _, err := run("new-session", "-d", "-s", session); err != nil {
		return err
	}
	applySessionOptions(session)
	return nil
}

// ListWindows returns the window names in a session.
func ListWindows(session string) ([]string, error) {
	out, err := run("list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// WindowExists reports whether a named window exists in the session. A read that
// TIMED OUT answers true; every other failure keeps answering false.
//
// The bool has nowhere to put "I could not tell", so an unanswered read has to fold
// into one of the two answers, and they are not equally safe. Almost every caller is
// shaped "if the window is not there, create it": Resume rebuilding a worker,
// Restore rebuilding the manager window, Spawn's pre-flight, the gate's idempotent
// reviewer launch. Folding a timeout to "gone" there starts a SECOND agent in a
// worktree that already has one, which is the worst outcome in this package. Folding
// it to "present" makes those callers skip, and makes the "refuse if absent" callers
// (Peek, Send) attempt the real command and fail with tmux's own error, which is a
// better message than a guess. It also matches how WindowExistsErr already treats a
// duplicate name: a window it cannot rule out is present, never gone.
//
// Only a timeout folds that way. The other failures are DEFINITE negatives, not
// uncertainty: `list-windows` against a session that does not exist yet fails, and
// so does a missing tmux, and in both cases the window really is absent. Folding
// those to "present" refused the first spawn on a machine with no tmux session —
// caught by the orchestrator suite, not by reasoning.
//
// Callers that want the full picture use WindowExistsErr; a timeout there is
// identifiable with errors.Is(err, ErrTimeout).
func WindowExists(session, window string) bool {
	exists, err := WindowExistsErr(session, window)
	return exists || errors.Is(err, ErrTimeout)
}

// WindowExistsErr reports whether a named window exists, distinguishing a window that
// is genuinely absent (false, nil) from a tmux read that failed (false, err). Callers
// that must not treat a transient `tmux list-windows` hiccup as "the window is gone"
// (e.g. Spawn's readiness wait) use this instead of WindowExists. It treats a name shared
// by MORE than one window as "exists" (true) — a duplicate is a present window, never a
// gone one — so a multi-match capture/send failure is never misread as a dead worker.
func WindowExistsErr(session, window string) (bool, error) {
	ws, err := ListWindows(session)
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if w == window {
			return true, nil
		}
	}
	return false, nil
}

// windowRef pairs a window's stable, server-unique id (@N) with its name, as read from one
// list-windows enumeration. The id — unlike a bare name (tmux permits duplicates) and unlike
// the positional index (reused as windows come and go) — addresses EXACTLY one window for the
// life of the server, so it is the unambiguous target dedup and multi-match kills use.
type windowRef struct {
	id   string
	name string
}

// listWindowRefs enumerates a session's windows as (id, name) pairs. Splitting on the first
// space is safe: a window_id is always "@<n>" (no space), and any space in the name stays with
// the name (ttorch window names never contain one). A read failure propagates so a transient
// hiccup is never mistaken for an empty session.
func listWindowRefs(session string) ([]windowRef, error) {
	out, err := run("list-windows", "-t", session, "-F", "#{window_id} #{window_name}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var refs []windowRef
	for _, line := range strings.Split(out, "\n") {
		id, name, ok := strings.Cut(line, " ")
		if !ok || id == "" {
			continue
		}
		refs = append(refs, windowRef{id: id, name: name})
	}
	return refs, nil
}

// WindowCount returns how many windows in the session carry the given name. tmux permits
// duplicate window names, so this distinguishes "absent" (0), the healthy "exactly one" (1),
// and the duplicate pathology (>1) that makes every name-targeted op (capture/send/kill)
// ambiguous — the explicit multi-match view WindowExists's bool cannot give. A read failure
// is surfaced (rather than reported as 0) so a transient hiccup is never read as "absent".
func WindowCount(session, name string) (int, error) {
	refs, err := listWindowRefs(session)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range refs {
		if r.name == name {
			n++
		}
	}
	return n, nil
}

// KillWindows removes EVERY window in the session that carries the given name and returns how
// many it killed. Where KillWindow targets by name — ambiguous, and so unreliable, the moment
// two windows share that name — this enumerates the matches and kills each by its server-unique
// window id (@N), the only target that stays unambiguous under duplicates. It is the dedup
// primitive NewWindow uses to guarantee at most one window per name, and the reliable cleanup
// for a duplicate a name-targeted KillWindow could not disambiguate. A failure to ENUMERATE
// (list-windows error) returns a zero count and that error, so a caller can tell "nothing to
// kill" from "could not look"; a per-window kill error is returned after attempting the rest.
func KillWindows(session, name string) (int, error) {
	refs, err := listWindowRefs(session)
	if err != nil {
		return 0, err
	}
	killed := 0
	var firstErr error
	for _, r := range refs {
		if r.name != name {
			continue
		}
		if _, err := run("kill-window", "-t", r.id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		killed++
	}
	return killed, firstErr
}

// NewWindow creates a detached window with the given working directory. The
// window's name is pinned (automatic-rename and allow-rename off) so the running
// command cannot overwrite it: ttorch uses the name as a stable tmux target and as
// a discovery key, while the friendly tab title is carried separately via the
// @ttorch_label window option (see LabelWindow and TitleFormat). Pinning is
// best-effort; only the window creation can fail the caller.
//
// It DEDUPES first: tmux's `new-window -n` does NOT enforce unique names, so a stale window
// left from a prior incarnation — e.g. a recovery re-spawn after a transient list-windows read
// made the caller's WindowExists guard miss the still-present window — would otherwise leave TWO
// windows sharing this name. Every name-targeted op (capture/send/kill) against such a pair then
// fails ambiguously, which a recovery pass can misread as "the worker is gone" and re-spawn yet
// again — a self-feeding duplicate-window loop. Killing any existing same-name window BEFORE
// creating guarantees at most one window of this name afterward, so a re-spawn REUSES (replaces)
// the stale window instead of stacking a duplicate on it. The dedup is best-effort: if the
// windows cannot be enumerated we still create (no worse than the un-deduped original), and the
// caller's own pre-check remains the first line of defense.
func NewWindow(session, window, cwd string) error {
	_, _ = KillWindows(session, window)
	if _, err := run("new-window", "-d", "-t", session, "-n", window, "-c", cwd); err != nil {
		return err
	}
	t := target(session, window)
	_, _ = run("set-option", "-w", "-t", t, "automatic-rename", "off")
	_, _ = run("set-option", "-w", "-t", t, "allow-rename", "off")
	return nil
}

// LabelWindow sets a window's friendly display title — the terminal tab name
// rendered by TitleFormat — via the @ttorch_label window option. It does not touch
// the window's tmux name, which stays the stable target. Best-effort.
func LabelWindow(session, window, label string) error {
	_, err := run("set-option", "-w", "-t", target(session, window), "@ttorch_label", label)
	return err
}

// Version returns the tmux version banner ("tmux 3.5a"), or "" if tmux cannot be
// run.
func Version() string {
	out, err := run("-V")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ReadOnlyViewFloor is the tmux version that introduced `new-session -f read-only`,
// which is what makes a worker view tab a view rather than a second keyboard.
// 'ttorch doctor' reports a tmux below it.
const ReadOnlyViewFloor = "3.2"

// BannerReadable reports whether a version banner is in a form ttorch can read a
// version out of. It separates "older than the floor" from "no idea", which
// callers answer differently: doctor reports an unreadable banner as unknown
// rather than as too old, and the view tab assumes such a build is modern.
func BannerReadable(banner string) bool {
	_, ok := parseVersion(banner)
	return ok
}

// BannerAtLeast reports whether a tmux version banner is at least major.minor.
// It accepts the forms tmux emits ("tmux 3.5a", "tmux 3.4", "tmux next-3.6") and
// answers false for anything it cannot read ("tmux master", a portable fork's own
// string, an empty banner), so a caller that needs a definite yes does not get one
// from a guess.
func BannerAtLeast(banner string, major, minor int) bool {
	got, ok := parseVersion(banner)
	if !ok {
		return false
	}
	return got[0] > major || (got[0] == major && got[1] >= minor)
}

// parseVersion pulls major and minor out of a tmux version banner.
func parseVersion(banner string) ([2]int, bool) {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(banner), "tmux"))
	v = strings.TrimPrefix(strings.TrimSpace(v), "next-")
	maj, rest, ok := strings.Cut(v, ".")
	if !ok {
		return [2]int{}, false
	}
	gotMaj, err := strconv.Atoi(maj)
	if err != nil {
		return [2]int{}, false
	}
	// The patch suffix is a letter ("3.5a"), so take the leading digits of the
	// minor field and drop the rest.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	gotMin, err := strconv.Atoi(rest[:end])
	if err != nil {
		return [2]int{}, false
	}
	return [2]int{gotMaj, gotMin}, true
}

// readOnlyViewSupport answers SupportsReadOnlyView. It probes once — the tmux on
// PATH cannot change under a running ttorch — and is a package var so a test can
// pin the answer rather than assert against whatever tmux the machine has.
var readOnlyViewSupport = sync.OnceValue(func() bool { return readOnlyViewSupported(Version()) })

// readOnlyViewSupported decides, from a version banner, whether to ask tmux for a
// read-only view client.
//
// An unreadable banner ("tmux master", a distro's own string) answers YES, which
// is the opposite of what a capability check usually does. The two outcomes are
// not symmetric. Guessing yes on a tmux too old for the flag costs a view tab that
// exits on the unknown flag: visible immediately, and the worker is untouched
// because it lives in the shared session. Guessing no hands the operator a
// writable second keyboard on a live agent that looks exactly like the fixed one.
// The only banners that answer no are the ones that parse and fall below the
// floor, where we positively know the flag is absent.
//
// This is safe to be generous about only because the steer path no longer depends
// on the same answer: sendKeys reacts to what tmux actually says rather than to a
// version guess.
func readOnlyViewSupported(banner string) bool {
	if !BannerReadable(banner) {
		return true
	}
	return BannerAtLeast(banner, 3, 2)
}

// SupportsReadOnlyView reports whether to attach worker view tabs with
// `new-session -f read-only` (package termtab). See readOnlyViewSupported.
func SupportsReadOnlyView() bool { return readOnlyViewSupport() }

// sendKeys sends keys to a pane without attributing them to any tmux client.
//
// With no -c, tmux resolves the target client to whichever client is *current* —
// in practice the most recently focused one — and refuses with "client is
// read-only" when that turns out to be a read-only client. The worker view tabs
// are read-only clients (package termtab), so an unattributed send would fail
// exactly when the lead was watching a worker, and fail always once the view tabs
// were the only clients left. An empty -c resolves to no client at all, so the
// keys reach the pane and nothing about which tab has focus can intercept them.
//
// tmux tolerates an unfound client here from 3.1 on; before that it rejects the
// send outright. Rather than guess from a version banner — which would make a
// misread banner silently kill the control channel — this tries the empty -c and
// falls back to the plain form on the one error that means the older behavior.
// On a modern tmux the fallback never runs.
func sendKeys(args ...string) error {
	out, err := run(append([]string{"send-keys", "-c", ""}, args...)...)
	if err != nil && strings.Contains(out, "can't find client") {
		_, err = run(append([]string{"send-keys"}, args...)...)
	}
	return err
}

// paneInMode reports whether a window's pane is in a tmux mode — copy-mode is the
// one that happens in practice, entered by anyone scrolling the pane. A diagnostic
// failure answers false: a broken check must not be able to block steering.
func paneInMode(session, window string) bool {
	out, err := run("display-message", "-p", "-t", target(session, window), "#{pane_in_mode}")
	return err == nil && strings.TrimSpace(out) == "1"
}

// SendLine types a line into a window then presses Enter. The text and the Enter
// are sent separately (a combined send can submit before a TUI has rendered the
// input); a short settle delay precedes Enter, longer for slash-commands which may
// open a completion popup.
//
// It refuses outright when the pane is in copy-mode, because a steer aimed at such
// a pane does not reach the agent and does not say so. The first attempt blocks
// until the run() deadline kills it; killing the client unwedges the server, so
// every attempt after that returns success in milliseconds while copy-mode eats the
// keys. Measured: after the deadline fired, a retry returned err=nil in 0.3s and the
// worker's stdin stayed empty. A steer that reports success and never arrived is how
// a worker quietly stops being steerable, so this fails loudly instead and names the
// key that fixes it. SendKey is deliberately not guarded — sending Escape is the way
// out of the mode.
func SendLine(session, window, text string) error {
	t := target(session, window)
	if paneInMode(session, window) {
		return fmt.Errorf("%s is in copy-mode, so a steer would be swallowed rather than reach the agent; press Escape in that pane (or run: tmux send-keys -X -t %s cancel) and retry", t, t)
	}
	if err := sendKeys("-t", t, "-l", text); err != nil {
		return err
	}
	delay := 300 * time.Millisecond
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		delay = 1200 * time.Millisecond
	}
	time.Sleep(delay)
	return sendKeys("-t", t, "Enter")
}

// SendKey sends a single named key (e.g. "Escape", "C-c") to a window.
func SendKey(session, window, key string) error {
	return sendKeys("-t", target(session, window), key)
}

// CapturePane returns the last n lines of a window's pane.
func CapturePane(session, window string, n int) (string, error) {
	return run("capture-pane", "-p", "-t", target(session, window), "-S", "-"+strconv.Itoa(n))
}

// PaneCurrentCommand returns the command name of the process in the foreground of a
// window's pane (tmux's #{pane_current_command}) — e.g. "zsh" while the window is
// still a bare shell, and "claude"/"node" once a harness has taken over. It returns
// "" if the window or pane cannot be read (e.g. the window has already exited). Spawn
// polls this to tell when a launched worker command is actually up before returning.
func PaneCurrentCommand(session, window string) string {
	out, err := run("display-message", "-p", "-t", target(session, window), "#{pane_current_command}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// PanePID returns the PID of the process running in a window's pane, or 0.
func PanePID(session, window string) int {
	out, err := run("list-panes", "-t", target(session, window), "-F", "#{pane_pid}")
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	pid, err := strconv.Atoi(line)
	if err != nil {
		return 0
	}
	return pid
}

// KillWindow removes a window (best effort).
func KillWindow(session, window string) error {
	_, err := run("kill-window", "-t", target(session, window))
	return err
}

// KillSession tears down an entire session and all its windows.
func KillSession(session string) error {
	_, err := run("kill-session", "-t", session)
	return err
}

// SelectWindow makes a window active.
func SelectWindow(session, window string) error {
	_, err := run("select-window", "-t", target(session, window))
	return err
}

// Attach focuses the window and hands the terminal to the session: switching the
// client if already inside tmux, otherwise replacing this process with
// `tmux attach`.
func Attach(session, window string) error {
	_ = SelectWindow(session, window)
	if InsideTmux() {
		_, err := run("switch-client", "-t", session)
		return err
	}
	b, err := bin()
	if err != nil {
		return err
	}
	return syscall.Exec(b, []string{"tmux", "attach-session", "-t", session}, os.Environ())
}
