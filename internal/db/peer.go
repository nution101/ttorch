package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// The peer registry (migration 0012): what a parent coordinator keeps about the peers it
// provisioned. Nothing here reaches a peer; internal/peer's client does, and the CLI records what
// it learned. A peer is never deleted: retiring one keeps its row, its repositories and its
// delegations.

// Peer statuses (migration 0012 CHECK).
const (
	PeerProvisioning = "provisioning"
	PeerLive         = "live"
	PeerUnreachable  = "unreachable"
	PeerRetired      = "retired"
)

// Delegation kinds (migration 0012 CHECK).
const (
	DelegationTask = "task"
	DelegationGoal = "goal"
)

// maxPeerField bounds a destination or key path stored for a peer.
const maxPeerField = 1024

// ErrPeerExists is a registration for a name a peer in use already has.
var ErrPeerExists = errors.New("a peer with that name is already registered")

// ErrPeerNotFound is a peer name nothing is registered under.
var ErrPeerNotFound = errors.New("no peer is registered with that name")

// ErrRepoOwned is a repository another coordinator already owns: a project of this one, or a
// repository of another peer in use.
var ErrRepoOwned = errors.New("that repository already belongs to a coordinator")

// Peer is one row of the registry. LastOKAt is zero when no call has succeeded.
type Peer struct {
	Name                string
	ControlDest         string
	ApproveDest         string
	ControlKey          string
	Status              string
	Protocol            int
	Version             string
	EscalationCursor    int64
	Summary             string
	ConsecutiveFailures int
	DownAttempts        int
	LastOKAt            time.Time
	LastError           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const peerColumns = `name, control_dest, approve_dest, control_key, status, protocol, version,
	escalation_cursor, summary, consecutive_failures, down_attempts, last_ok_at, last_error,
	created_at, updated_at`

func scanPeer(r rowScanner) (Peer, error) {
	var (
		p                Peer
		lastOK           sql.NullString
		created, updated string
	)
	if err := r.Scan(&p.Name, &p.ControlDest, &p.ApproveDest, &p.ControlKey, &p.Status, &p.Protocol,
		&p.Version, &p.EscalationCursor, &p.Summary, &p.ConsecutiveFailures, &p.DownAttempts, &lastOK,
		&p.LastError, &created, &updated); err != nil {
		return Peer{}, err
	}
	var err error
	if lastOK.Valid {
		if p.LastOKAt, err = parseTime(lastOK.String); err != nil {
			return Peer{}, err
		}
	}
	if p.CreatedAt, err = parseTime(created); err != nil {
		return Peer{}, err
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return Peer{}, err
	}
	return p, nil
}

func getPeer(ctx context.Context, q queryer, name string) (Peer, bool, error) {
	p, err := scanPeer(q.QueryRowContext(ctx, `SELECT `+peerColumns+` FROM peers WHERE name = ?`, name))
	if err == sql.ErrNoRows {
		return Peer{}, false, nil
	}
	if err != nil {
		return Peer{}, false, err
	}
	return p, true, nil
}

// GetPeer reads one peer, and whether it is registered.
func (s *Store) GetPeer(ctx context.Context, name string) (Peer, bool, error) {
	return getPeer(ctx, s.db, name)
}

// ListPeers reads every peer, retired ones included, by name.
func (s *Store) ListPeers(ctx context.Context) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+peerColumns+` FROM peers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// printableField refuses an empty value, one over maxPeerField bytes, or one holding anything
// that is not a printable rune.
func printableField(what, v string) error {
	if v == "" || len(v) > maxPeerField {
		return fmt.Errorf("%s must be 1 to %d bytes", what, maxPeerField)
	}
	if strings.ToValidUTF8(v, "�") != v || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) || unicode.IsSpace(r) }) >= 0 {
		return fmt.Errorf("%s holds whitespace or a non-printing character", what)
	}
	return nil
}

// RegisterPeer records a peer that is being provisioned, with status provisioning. A new name
// gets a new row. An existing row is updated with p's destinations and key and set back to
// provisioning when it is retired, when it is still provisioning (a retried `peer add`), or when
// replace is set (`peer adopt`); otherwise the name is in use and the call fails with
// ErrPeerExists. The row's creation time, its repositories and its delegations are kept. A
// coordinator that is itself a peer registers none (ErrCoordinatorIsPeer).
func (s *Store) RegisterPeer(ctx context.Context, p Peer, replace bool) (Peer, error) {
	if err := ValidPeerName(p.Name); err != nil {
		return Peer{}, err
	}
	if err := printableField("the control destination", p.ControlDest); err != nil {
		return Peer{}, err
	}
	if err := printableField("the approve destination", p.ApproveDest); err != nil {
		return Peer{}, err
	}
	if err := printableField("the control key path", p.ControlKey); err != nil {
		return Peer{}, err
	}
	if !filepath.IsAbs(p.ControlKey) {
		return Peer{}, errors.New("the control key path must be absolute")
	}
	now := formatTime(s.now())
	var out Peer
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// The other end of ProvisionAsPeer's ErrHasPeers, in the same kind of transaction, so a
		// peer add and a peer init racing on one store cannot both pass.
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM coordinator WHERE id = 1`).Scan(&role); err != nil {
			return err
		}
		if role == CoordinatorPeer {
			return ErrCoordinatorIsPeer
		}
		cur, ok, err := getPeer(ctx, tx, p.Name)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			_, err = tx.ExecContext(ctx,
				`INSERT INTO peers (name, control_dest, approve_dest, control_key, status, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.Name, p.ControlDest, p.ApproveDest, p.ControlKey, PeerProvisioning, now, now)
		case cur.Status == PeerRetired, cur.Status == PeerProvisioning, replace:
			_, err = tx.ExecContext(ctx,
				`UPDATE peers SET control_dest = ?, approve_dest = ?, control_key = ?, status = ?,
				 last_error = '', updated_at = ? WHERE name = ?`,
				p.ControlDest, p.ApproveDest, p.ControlKey, PeerProvisioning, now, p.Name)
		default:
			return fmt.Errorf("peer %s is %s: %w", p.Name, cur.Status, ErrPeerExists)
		}
		if err != nil {
			return err
		}
		out, _, err = getPeer(ctx, tx, p.Name)
		return err
	})
	if err != nil {
		return Peer{}, err
	}
	return out, nil
}

// updatePeer runs one UPDATE against name's row, refusing a name nothing is registered under.
func (s *Store) updatePeer(ctx context.Context, name, set string, args ...any) error {
	res, err := s.db.ExecContext(ctx, `UPDATE peers SET `+set+` WHERE name = ?`, append(args, name)...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("peer %s: %w", name, ErrPeerNotFound)
	}
	return nil
}

// MarkPeerLive records that the peer's control key answered `version` with this protocol and
// binary version: status live, the call's time as the last success, no error, no failure streak.
// A retired peer is refused; registering it again comes first.
func (s *Store) MarkPeerLive(ctx context.Context, name string, protocol int, version string) error {
	version, _ = CapText(version, maxPeerField)
	now := formatTime(s.now())
	return s.withTx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := getPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("peer %s: %w", name, ErrPeerNotFound)
		}
		if cur.Status == PeerRetired {
			return fmt.Errorf("peer %s is retired; register it again with ttorch peer add", name)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE peers SET status = ?, protocol = ?, version = ?, last_ok_at = ?, last_error = '',
			 consecutive_failures = 0, updated_at = ? WHERE name = ?`,
			PeerLive, protocol, version, now, now, name)
		return err
	})
}

// RecordPeerOK records a successful call: its time as the last success, no error, and, when
// summary is set, the summary it read as the display cache.
func (s *Store) RecordPeerOK(ctx context.Context, name, summary string) error {
	now := formatTime(s.now())
	if summary == "" {
		return s.updatePeer(ctx, name, `last_ok_at = ?, last_error = '', updated_at = ?`, now, now)
	}
	return s.updatePeer(ctx, name, `summary = ?, last_ok_at = ?, last_error = '', updated_at = ?`, summary, now, now)
}

// RecordPeerError records why a call failed, capped at MaxEscalationText. The caller escapes it;
// the status is the scheduler's poll to change, not one failed call's.
func (s *Store) RecordPeerError(ctx context.Context, name, msg string) error {
	msg, _ = CapText(msg, MaxEscalationText)
	return s.updatePeer(ctx, name, `last_error = ?, updated_at = ?`, msg, formatTime(s.now()))
}

// RetirePeer stops using a peer: status retired. Its row, repositories and delegations stay.
// Retiring a retired peer changes nothing.
func (s *Store) RetirePeer(ctx context.Context, name string) (Peer, error) {
	if err := s.updatePeer(ctx, name, `status = ?, updated_at = ?`, PeerRetired, formatTime(s.now())); err != nil {
		return Peer{}, err
	}
	p, _, err := s.GetPeer(ctx, name)
	return p, err
}

// PeerRepo is a repository a peer owns: its path on the peer and its origin URL.
type PeerRepo struct {
	Peer       string
	RemotePath string
	OriginURL  string
}

// OriginKey is the form two origin URLs are compared in: host and path, without a user, port,
// scheme, trailing slash or .git suffix, lowercased. git@host:org/app.git,
// ssh://git@host/org/app and https://host/org/app/ all give host/org/app. A local path is kept as
// a path. It is for telling that two URLs name one repository, not for fetching anything.
func OriginKey(origin string) string {
	o := strings.TrimSpace(origin)
	switch {
	case strings.Contains(o, "://"):
		if u, err := url.Parse(o); err == nil {
			o = u.Hostname() + "/" + strings.TrimPrefix(u.Path, "/")
		}
	case !strings.HasPrefix(o, "/") && strings.Contains(o, ":"):
		// scp-like: [user@]host:path, with no slash before the colon.
		host, path, _ := strings.Cut(o, ":")
		if !strings.Contains(host, "/") {
			if _, h, ok := strings.Cut(host, "@"); ok {
				host = h
			}
			o = host + "/" + strings.TrimPrefix(path, "/")
		}
	}
	o = strings.TrimRight(o, "/")
	o = strings.TrimSuffix(o, ".git")
	return strings.ToLower(strings.TrimRight(o, "/"))
}

// AddPeerRepo records that peer owns the repository at remotePath on it, whose origin is
// originURL. One repository belongs to one coordinator (design 3.5), so it is refused with
// ErrRepoOwned when an active project of this coordinator has the same origin, or a peer other
// than a retired one already has it, this one at another path included. Only what this store
// knows is checked. The same path with the same origin again is accepted as it is; with another
// origin it is refused. A retired peer takes no repository.
func (s *Store) AddPeerRepo(ctx context.Context, peer, remotePath, originURL string) (PeerRepo, error) {
	if err := printableField("the repository path", remotePath); err != nil {
		return PeerRepo{}, err
	}
	if !strings.HasPrefix(remotePath, "/") {
		return PeerRepo{}, errors.New("the repository path must be absolute on the peer")
	}
	originURL = strings.TrimSpace(originURL)
	if err := printableField("the origin URL", originURL); err != nil {
		return PeerRepo{}, err
	}
	key := OriginKey(originURL)
	out := PeerRepo{Peer: peer, RemotePath: remotePath, OriginURL: originURL}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, ok, err := getPeer(ctx, tx, peer)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("peer %s: %w", peer, ErrPeerNotFound)
		}
		if p.Status == PeerRetired {
			return fmt.Errorf("peer %s is retired", peer)
		}
		rows, err := tx.QueryContext(ctx, `SELECT repo_path, origin_url FROM projects WHERE status != 'archived' AND origin_url != ''`)
		if err != nil {
			return err
		}
		var owner string
		for rows.Next() {
			var path, origin string
			if err := rows.Scan(&path, &origin); err != nil {
				rows.Close()
				return err
			}
			if owner == "" && OriginKey(origin) == key {
				owner = "this coordinator's project " + path
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx,
			`SELECT r.peer, r.remote_path, r.origin_url FROM peer_repos r JOIN peers p ON p.name = r.peer
			 WHERE p.status != ?`, PeerRetired)
		if err != nil {
			return err
		}
		var same *PeerRepo
		for rows.Next() {
			var r PeerRepo
			if err := rows.Scan(&r.Peer, &r.RemotePath, &r.OriginURL); err != nil {
				rows.Close()
				return err
			}
			switch {
			case r.Peer == peer && r.RemotePath == remotePath:
				same = &r
			case owner == "" && OriginKey(r.OriginURL) == key:
				owner = "peer " + r.Peer + "'s repository " + r.RemotePath
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if same != nil {
			if OriginKey(same.OriginURL) != key {
				return fmt.Errorf("peer %s already has %s, with origin %s", peer, remotePath, same.OriginURL)
			}
			out = *same
			return nil
		}
		if owner != "" {
			return fmt.Errorf("%s has origin %s: %w", owner, originURL, ErrRepoOwned)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO peer_repos (peer, remote_path, origin_url) VALUES (?, ?, ?)`, peer, remotePath, originURL)
		return err
	})
	if err != nil {
		return PeerRepo{}, err
	}
	return out, nil
}

// ListPeerRepos reads the repositories peer owns, by path.
func (s *Store) ListPeerRepos(ctx context.Context, peer string) ([]PeerRepo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer, remote_path, origin_url FROM peer_repos WHERE peer = ? ORDER BY remote_path`, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerRepo
	for rows.Next() {
		var r PeerRepo
		if err := rows.Scan(&r.Peer, &r.RemotePath, &r.OriginURL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delegation is one task-add or goal the parent sent a peer, under the request id it minted.
// RemoteTaskID is the task's id on the peer, "" for a goal; BriefSHA256 is the hex SHA-256 of the
// brief or the goal's text.
type Delegation struct {
	RequestID    string
	Peer         string
	Kind         string
	RemoteTaskID string
	BriefSHA256  string
	CreatedAt    time.Time
}

// RecordDelegation records d before it is sent, so a parent that stops mid-call still knows what
// it may have asked for. A repeat of the request id for the same peer, kind, task and hash
// returns the first record with existed set; one that differs in any of them is refused with
// ErrRequestReused.
func (s *Store) RecordDelegation(ctx context.Context, d Delegation) (out Delegation, existed bool, err error) {
	if err := ValidRequestID(d.RequestID); err != nil {
		return Delegation{}, false, err
	}
	switch {
	case d.Kind == DelegationTask && d.RemoteTaskID == "":
		return Delegation{}, false, errors.New("a task delegation names the task")
	case d.Kind == DelegationGoal && d.RemoteTaskID != "":
		return Delegation{}, false, errors.New("a goal delegation names no task")
	case d.Kind != DelegationTask && d.Kind != DelegationGoal:
		return Delegation{}, false, fmt.Errorf("a delegation is a %s or a %s, not %q", DelegationTask, DelegationGoal, d.Kind)
	}
	now := s.now()
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			cur     Delegation
			created string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT request_id, peer, kind, remote_task_id, brief_sha256, created_at FROM peer_delegations WHERE request_id = ?`,
			d.RequestID).Scan(&cur.RequestID, &cur.Peer, &cur.Kind, &cur.RemoteTaskID, &cur.BriefSHA256, &created)
		if err == nil {
			if cur.Peer != d.Peer || cur.Kind != d.Kind || cur.RemoteTaskID != d.RemoteTaskID || cur.BriefSHA256 != d.BriefSHA256 {
				return fmt.Errorf("request id %s was sent to %s as a %s for %q: %w", d.RequestID, cur.Peer, cur.Kind, cur.RemoteTaskID, ErrRequestReused)
			}
			if cur.CreatedAt, err = parseTime(created); err != nil {
				return err
			}
			out, existed = cur, true
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, ok, err := getPeer(ctx, tx, d.Peer); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("peer %s: %w", d.Peer, ErrPeerNotFound)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO peer_delegations (request_id, peer, kind, remote_task_id, brief_sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			d.RequestID, d.Peer, d.Kind, d.RemoteTaskID, d.BriefSHA256, formatTime(now)); err != nil {
			return err
		}
		out = d
		out.CreatedAt, err = parseTime(formatTime(now))
		return err
	})
	if err != nil {
		return Delegation{}, false, err
	}
	return out, existed, nil
}

// ForgetDelegation drops the record of a request the peer refused, so the registry lists only
// what may have been delegated. A request id with no record is not an error.
func (s *Store) ForgetDelegation(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM peer_delegations WHERE request_id = ?`, requestID)
	return err
}

// ListDelegations reads what was delegated to peer, oldest first.
func (s *Store) ListDelegations(ctx context.Context, peer string) ([]Delegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id, peer, kind, remote_task_id, brief_sha256, created_at FROM peer_delegations
		 WHERE peer = ? ORDER BY created_at, request_id`, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delegation
	for rows.Next() {
		var (
			d       Delegation
			created string
		)
		if err := rows.Scan(&d.RequestID, &d.Peer, &d.Kind, &d.RemoteTaskID, &d.BriefSHA256, &created); err != nil {
			return nil, err
		}
		if d.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DelegationCount is how many tasks and goals were delegated to one peer.
type DelegationCount struct{ Tasks, Goals int }

// CountDelegations counts what was delegated to each peer. A peer with none is absent.
func (s *Store) CountDelegations(ctx context.Context) (map[string]DelegationCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer, kind, count(*) FROM peer_delegations GROUP BY peer, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DelegationCount{}
	for rows.Next() {
		var (
			peer, kind string
			n          int
		)
		if err := rows.Scan(&peer, &kind, &n); err != nil {
			return nil, err
		}
		c := out[peer]
		if kind == DelegationTask {
			c.Tasks = n
		} else {
			c.Goals = n
		}
		out[peer] = c
	}
	return out, rows.Err()
}
