package scheduler

// The peer pass: a root coordinator polls each peer it provisioned for its summary and its open
// escalations, turns each new escalation into one actionable event here for the manager, and
// handles a peer that stops answering or comes back without a manager (design sections 4.4, 5.5
// and 5.6).
//
// It runs on its own cadence, PeerPoll (TTORCH_PEER_POLL, default 30s), and never inside the
// tick: runTick only starts it when it is due, and each peer is polled in a goroutine of its own
// under a deadline. So a peer that never answers holds up neither the tick nor the other peers.
// A peer still being polled when the next pass is due is skipped by that pass rather than polled
// twice.
//
// Everything a peer sends is untrusted. The control client (internal/peer) already escapes and
// caps every string it decodes; this pass escapes again anything that still holds a non-printing
// rune and caps each event payload at 2 KiB as a whole, so a channel that skipped the first step
// still cannot put raw text in front of the manager. The state the pass keeps (the escalation
// cursor, the failure streak, the down episode) is on the peers row, and each change to it is one
// transaction with the event it raises (db.RecordPeerPoll, RecordPeerFailure, StepPeerDown).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nution101/ttorch/internal/db"
)

// DefaultPeerPoll is the peer pass's cadence when TTORCH_PEER_POLL is unset. A peer's decisions
// wait on a person, who answers in minutes, so 30s is well inside that.
const DefaultPeerPoll = 30 * time.Second

// defaultPeerDeadline bounds one peer's whole poll: a summary, a decisions call and, for a down
// peer, an ensure-up, each already bounded by the client's own deadline (30s).
const defaultPeerDeadline = 90 * time.Second

// maxPeerRaisesPerPoll bounds how many escalations one poll of one peer raises. The cursor moves
// only past what was raised, so the rest arrive on the next polls, none lost.
const maxPeerRaisesPerPoll = 32

// maxPeerEscalationID is the largest escalation id a poll accepts. A peer's ids count its
// escalations from 1, so an id past this one, which is more than one a second for 68 years, is
// a broken or hostile peer, and accepting it would put the cursor above every id the peer will
// ever really use.
const maxPeerEscalationID = 1<<31 - 1

// maxPeerEscalationJump is how far above the cursor a new escalation id may be. Ids grow by one
// per escalation and a poll runs every 30s, so a gap this wide since the last poll is not a busy
// peer; the id is refused and a peer_protocol_error raised rather than the cursor moved. A peer
// polled for the first time (cursor 0) has no bound but maxPeerEscalationID.
const maxPeerEscalationJump = 10000

// peerPollBounds are the limits each poll records against (db.PeerPollBounds).
var peerPollBounds = db.PeerPollBounds{MaxID: maxPeerEscalationID, MaxJump: maxPeerEscalationJump, MaxRaise: maxPeerRaisesPerPoll}

// Caps on the peer's strings in an event payload, after escaping.
const (
	maxPeerPayload = db.MaxEscalationText // the whole payload
	maxPeerTaskID  = 128                  // a task id, as the control channel caps one
	maxPeerKind    = 64                   // an escalation kind
)

const envPeerPoll = "TTORCH_PEER_POLL"

// PeerSummary is what the pass reads from a peer's summary: whether its manager window is up and
// its scheduler running and ticking, as the peer itself judged them, and the summary as JSON to
// cache on the peers row for display.
type PeerSummary struct {
	ManagerWindow    bool
	SchedulerRunning bool
	SchedulerStalled bool
	JSON             string
}

// healthy is a summary that needs no ensure-up: a manager window and a scheduler that ticks.
func (s PeerSummary) healthy() bool {
	return s.ManagerWindow && s.SchedulerRunning && !s.SchedulerStalled
}

// PeerEscalation is one escalation open on a peer, as its decisions verb lists it. Every string
// in it came from the peer. CreatedAt is the peer's own stamp for it, which with ID names it:
// the pass never compares it with this machine's clock.
type PeerEscalation struct {
	ID        int64
	Kind      string
	TaskID    string
	Body      string
	CreatedAt time.Time
}

// PeerDecisions is a peer's answer to decisions: how many escalations it has open, and the open
// ones above the id the pass asked from (the pass asks for all of them).
type PeerDecisions struct {
	Open        int
	Escalations []PeerEscalation
}

// PeerChannel is one peer's control channel, as the pass uses it. The CLI wires the control
// client (internal/peer.Client), which runs one ssh process per call through the control key
// alone and kills it at the client's deadline. A call returns when ctx is done.
type PeerChannel interface {
	Summary(ctx context.Context) (PeerSummary, error)
	Decisions(ctx context.Context, since int64) (PeerDecisions, error)
	EnsureUp(ctx context.Context) (string, error)
}

// PeerDialer opens the control channel to a registered peer.
type PeerDialer func(p db.Peer) PeerChannel

// peerPassState is the pass's in-memory state: when the next pass is due, which peers have a poll
// in flight, and the goroutines Run waits for. It is not the pass's record; the peers rows are,
// so a restart costs nothing but an early first pass.
type peerPassState struct {
	mu       sync.Mutex
	next     time.Time
	inFlight map[string]bool
	wg       sync.WaitGroup
}

// peerPollFromEnv resolves the peer pass's cadence from TTORCH_PEER_POLL (a Go duration such as
// "1m"), falling back to DefaultPeerPoll when it is unset or unparseable. Zero or less turns the
// pass off.
func peerPollFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv(envPeerPoll)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			if d < 0 {
				return 0
			}
			return d
		}
	}
	return DefaultPeerPoll
}

// startPeerPass starts a peer pass in the background when one is due, and returns at once. It is
// the tick's only part in the pass, so a peer that never answers costs the tick nothing.
func (sc *Scheduler) startPeerPass(ctx context.Context) {
	if sc.Peers == nil || sc.PeerPoll <= 0 || ctx.Err() != nil {
		return
	}
	now := sc.clock()
	st := &sc.peerPass
	st.mu.Lock()
	due := !now.Before(st.next)
	if due {
		st.next = now.Add(sc.PeerPoll)
	}
	st.mu.Unlock()
	if !due {
		return
	}
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		if _, err := sc.RunPeerPassOnce(ctx); err != nil && ctx.Err() == nil {
			sc.logf("peer pass error: %v", err)
		}
	}()
}

// waitPeerPass waits for every pass startPeerPass started to finish.
func (sc *Scheduler) waitPeerPass() { sc.peerPass.wg.Wait() }

// RunPeerPassOnce polls every live or unreachable peer once, each in its own goroutine, and waits
// for them. A peer whose poll from an earlier pass is still in flight is skipped. It returns how
// many peers it polled; a peer's failure is recorded on its row, not returned.
func (sc *Scheduler) RunPeerPassOnce(ctx context.Context) (int, error) {
	if sc.Peers == nil {
		return 0, nil
	}
	peers, err := sc.Store.ListPeers(ctx)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	n := 0
	for _, p := range peers {
		if p.Status != db.PeerLive && p.Status != db.PeerUnreachable {
			continue
		}
		if !sc.claimPeerPoll(p.Name) {
			continue
		}
		n++
		wg.Add(1)
		go func(p db.Peer) {
			defer wg.Done()
			defer sc.releasePeerPoll(p.Name)
			sc.pollPeer(ctx, p)
		}(p)
	}
	wg.Wait()
	return n, nil
}

func (sc *Scheduler) claimPeerPoll(name string) bool {
	st := &sc.peerPass
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.inFlight[name] {
		return false
	}
	if st.inFlight == nil {
		st.inFlight = map[string]bool{}
	}
	st.inFlight[name] = true
	return true
}

func (sc *Scheduler) releasePeerPoll(name string) {
	st := &sc.peerPass
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.inFlight, name)
}

// pollPeer polls one peer under its deadline: summary, then decisions for every open escalation.
// A poll that reaches the peer is recorded with the escalations it raises (db.RecordPeerPoll
// decides which, against what was already raised, and spots ids that went back or are out of
// range); one that fails counts toward unreachable. A peer that answers but shows no manager window or no ticking scheduler then takes
// the next step of its down episode: ensure-up, or once those are spent, peer_down. The store is
// written under ctx, not the poll's deadline, so a poll cut off at its deadline is still
// recorded; a poll cut off by shutdown is not, since that is no failure of the peer's.
func (sc *Scheduler) pollPeer(ctx context.Context, p db.Peer) {
	deadline := sc.PeerDeadline
	if deadline <= 0 {
		deadline = defaultPeerDeadline
	}
	pctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ch := sc.Peers(p)
	if ch == nil {
		return
	}
	sum, err := ch.Summary(pctx)
	var dec PeerDecisions
	if err == nil {
		// The whole open list, not just what is above the cursor: an escalation at or below
		// it that was never raised here is how a store that went back shows.
		dec, err = ch.Decisions(pctx, 0)
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		msg := untrustedText(err.Error(), db.MaxEscalationText)
		payload := db.PeerUnreachablePayload{Peer: p.Name, FailedPolls: db.PeerUnreachableAfter, Error: msg}
		res, rerr := sc.Store.RecordPeerFailure(ctx, p.Name, msg, fitPayload(&payload, &payload.Error))
		switch {
		case rerr != nil:
			sc.logPeerStoreErr(p.Name, rerr)
		case res.Unreachable:
			sc.logf("peer %s unreachable after %d failed polls (event %d): %s", p.Name, res.Failures, res.EventID, msg)
		}
		return
	}

	poll := db.PeerPoll{
		Summary: sum.JSON, Healthy: sum.healthy(), Bounds: peerPollBounds,
		RecoveredPayload: fitPayload(db.PeerRecoveredPayload{Peer: p.Name}, nil),
		PayloadFor:       func(i int) string { return escalationPayload(p.Name, dec.Escalations[i], dec.Open) },
	}
	for _, e := range dec.Escalations {
		poll.Open = append(poll.Open, db.PeerOpenEscalation{ID: e.ID, CreatedAt: stamp(e.CreatedAt)})
	}
	res, err := sc.Store.RecordPeerPoll(ctx, p.Name, poll)
	if err != nil {
		sc.logPeerStoreErr(p.Name, err)
		return
	}
	if res.Recovered {
		sc.logf("peer %s answered again (event %d)", p.Name, res.RecoveredEventID)
	}
	if res.CursorReset {
		sc.logf("peer %s: its escalation ids went back (a recreated or restored store); cursor re-synced to %d (event %d)", p.Name, res.Cursor, res.ResetEventID)
	}
	if res.ProtocolEventID != 0 {
		sc.logf("peer %s: refused %d escalation id(s) out of range (event %d)", p.Name, len(res.Refused), res.ProtocolEventID)
	}
	if len(res.Raised) > 0 {
		sc.logf("peer %s: raised %d escalation(s), cursor now %d", p.Name, len(res.Raised), res.Cursor)
	}
	if poll.Healthy {
		return
	}
	step, err := sc.Store.StepPeerDown(ctx, p.Name, fitPayload(db.PeerDownPayload{
		Peer: p.Name, EnsureUpCalls: db.PeerEnsureUpAttempts, ManagerWindow: sum.ManagerWindow,
		SchedulerRunning: sum.SchedulerRunning, SchedulerStalled: sum.SchedulerStalled,
	}, nil))
	switch {
	case err != nil:
		sc.logPeerStoreErr(p.Name, err)
	case step.Down:
		sc.logf("peer %s is down after %d ensure-up calls (event %d)", p.Name, db.PeerEnsureUpAttempts, step.EventID)
	case step.EnsureUp:
		out, err := ch.EnsureUp(pctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			msg := untrustedText(err.Error(), db.MaxEscalationText)
			_ = sc.Store.RecordPeerError(ctx, p.Name, msg)
			sc.logf("peer %s: ensure-up %d of %d failed: %s", p.Name, step.Attempt, db.PeerEnsureUpAttempts, msg)
			return
		}
		sc.logf("peer %s: ensure-up %d of %d: %s", p.Name, step.Attempt, db.PeerEnsureUpAttempts, untrustedText(out, db.MaxEscalationText))
	}
}

func (sc *Scheduler) logPeerStoreErr(name string, err error) {
	if errors.Is(err, db.ErrPeerNotPolled) {
		return // retired or re-provisioned while the poll ran
	}
	sc.logf("peer %s: recording the poll: %v", name, err)
}

// stamp is the peer's creation stamp for an escalation as the ledger keeps it. A zero time (a
// peer that sent none) is "", so such escalations are told apart by id alone.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// escalationPayload is the payload of e's peer_escalation event: the peer, the escalation's id,
// kind and task, the peer's creation stamp, how many the peer has open, and its body, all
// escaped and capped.
func escalationPayload(peer string, e PeerEscalation, open int) string {
	p := db.PeerEscalationPayload{
		Peer: peer, EscalationID: e.ID, Kind: untrustedText(e.Kind, maxPeerKind),
		TaskID: untrustedText(e.TaskID, maxPeerTaskID), CreatedAt: stamp(e.CreatedAt), Open: open,
		Body: untrustedText(e.Body, maxPeerPayload),
	}
	return fitPayload(&p, &p.Body)
}

// fitPayload encodes v, one of the db.Peer*Payload types, as one JSON object of at most
// maxPeerPayload bytes. When it is over, the string cut points at (a field of v) is shortened
// until it fits; the other fields are small and capped where they were built. cut nil means v
// has no free text. HTML is not escaped, so the cap is spent on the text.
func fitPayload(v any, cut *string) string {
	for {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return ""
		}
		out := strings.TrimSuffix(b.String(), "\n")
		over := len(out) - maxPeerPayload
		if over <= 0 || cut == nil {
			return out
		}
		shorter := capWithMarker(*cut, len(*cut)-over)
		if len(shorter) >= len(*cut) {
			return out // nothing left to cut; the other fields are capped where they were built
		}
		*cut = shorter
	}
}

// truncatedMark ends a string a cap cut short.
const truncatedMark = "…"

// untrustedText makes text from a peer safe to show: invalid UTF-8 becomes U+FFFD, and text that
// still holds a rune that is not graphic (a C0, C1 or DEL control, a bidi override or another
// format character) is written out as a Go string literal would write it. Text with nothing to
// escape, which is what the control client hands over, is left as it is, so it is not escaped
// twice. The result is capped at n bytes.
func untrustedText(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		q := strconv.QuoteToGraphic(s)
		s = q[1 : len(q)-1]
	}
	return capWithMarker(s, n)
}

// capWithMarker cuts s to at most n bytes on a rune boundary, ending a cut string with
// truncatedMark.
func capWithMarker(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= len(truncatedMark) {
		return truncatedMark
	}
	cut, _ := db.CapText(s, n-len(truncatedMark))
	return cut + truncatedMark
}
