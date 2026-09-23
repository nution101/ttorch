package db

import "context"

// RecentTaskEvents returns up to limit task-scoped events whose type is one of types,
// newest first. It reads the event spine across every task, which the per-task Timeline
// cannot do without one query per task; the board uses it for its recent-completions list
// (merged / delivered / pr_merged). An empty types list or a non-positive limit returns
// nothing rather than every event.
func (s *Store) RecentTaskEvents(ctx context.Context, types []string, limit int) ([]Event, error) {
	if len(types) == 0 || limit <= 0 {
		return nil, nil
	}
	args := make([]any, 0, len(types)+1)
	for _, t := range types {
		args = append(args, t)
	}
	args = append(args, limit)
	return s.collectEvents(ctx,
		`SELECT `+eventColumns+` FROM events
		  WHERE entity_type = 'task' AND type IN (`+placeholders(len(types))+`)
		  ORDER BY id DESC LIMIT ?`, args...)
}
