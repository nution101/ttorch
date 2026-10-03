package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Coordinator roles (migration 0011 CHECK). A store starts as root; provisioning a peer
// rewrites the row.
const (
	CoordinatorRoot = "root"
	CoordinatorPeer = "peer"
)

// Coordinator is this store's identity row: a random id minted by migration 0011, whether it
// is a root or a peer, the name a parent registered it under, and the id of the parent that
// provisioned it ("" for a root). Any process running as the store's user can rewrite the row,
// so it identifies, and authorizes nothing.
type Coordinator struct {
	CoordID   string
	Role      string
	Name      string
	ParentID  string
	CreatedAt time.Time
}

// GetCoordinator reads the identity row. Migration 0011 inserts it, so a migrated store always
// has one.
func (s *Store) GetCoordinator(ctx context.Context) (Coordinator, error) {
	var (
		c       Coordinator
		created string
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT coord_id, role, name, parent_id, created_at FROM coordinator WHERE id = 1`,
	).Scan(&c.CoordID, &c.Role, &c.Name, &c.ParentID, &created); err != nil {
		return Coordinator{}, err
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Coordinator{}, err
	}
	return c, nil
}

// MaxPeerName bounds a peer's name: the name a parent registers it under, which also names its
// directory under the parent's ttorch home.
const MaxPeerName = 32

// ValidPeerName refuses a peer name that is not 1 to MaxPeerName lowercase letters, digits and
// hyphens starting with a letter or digit. It names a directory and appears in event ids, so
// nothing in it may be a path separator, whitespace or a shell character.
func ValidPeerName(name string) error {
	if name == "" || len(name) > MaxPeerName {
		return fmt.Errorf("a peer name must be 1 to %d bytes, got %d", MaxPeerName, len(name))
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return fmt.Errorf("a peer name holds only lowercase letters, digits and hyphens, and starts with a letter or digit; got %q", name)
		}
	}
	return nil
}

// ValidCoordID refuses anything but a coordinator id as migration 0011 mints one: 32 lowercase
// hex digits.
func ValidCoordID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("a coordinator id is 32 hex digits, got %d bytes", len(id))
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("a coordinator id is 32 lowercase hex digits, got %q", id)
		}
	}
	return nil
}

// ErrOtherParent is provisioning by a parent other than the one the coordinator row records.
var ErrOtherParent = errors.New("this coordinator was provisioned by another parent")

// Provisioned is the coordinator row after ProvisionAsPeer, and the parent it recorded before
// ("" when none).
type Provisioned struct {
	Coordinator    Coordinator
	PreviousParent string
}

// ProvisionAsPeer makes this store a peer of the parent coordinator parentID, registered there
// as name: role peer, the name, and the parent's id, which the control channel then requires on
// every request that changes state. The coordinator's own id is kept.
//
// A store a parent already provisioned keeps that parent: another one is refused with
// ErrOtherParent and nothing changes, unless force is set, which is `ttorch peer adopt --force`
// moving the peer. The same parent may provision it again, under the same name or another.
//
// The row identifies and authorizes nothing (see Coordinator): any process running as this
// store's user can rewrite it. The refusal keeps two parents from relaying one peer's work by
// accident.
func (s *Store) ProvisionAsPeer(ctx context.Context, name, parentID string, force bool) (Provisioned, error) {
	if err := ValidPeerName(name); err != nil {
		return Provisioned{}, err
	}
	if err := ValidCoordID(parentID); err != nil {
		return Provisioned{}, fmt.Errorf("parent: %w", err)
	}
	var out Provisioned
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var role, previous string
		if err := tx.QueryRowContext(ctx, `SELECT role, parent_id FROM coordinator WHERE id = 1`).Scan(&role, &previous); err != nil {
			return err
		}
		if role == CoordinatorPeer && previous != "" && previous != parentID && !force {
			return fmt.Errorf("%w %s; moving it to this one is ttorch peer adopt --force", ErrOtherParent, previous)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE coordinator SET role = ?, name = ?, parent_id = ? WHERE id = 1`,
			CoordinatorPeer, name, parentID); err != nil {
			return err
		}
		out.PreviousParent = previous
		return nil
	})
	if err != nil {
		return Provisioned{}, err
	}
	if out.Coordinator, err = s.GetCoordinator(ctx); err != nil {
		return Provisioned{}, err
	}
	return out, nil
}
