package db

import (
	"context"
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
