package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Legacy on-disk layout (pre-SQLite). These are the file names and JSON shapes the
// retired internal/state package wrote; the importer reads them WITHOUT importing
// internal/state (whose persistence is being removed), so the migration carries its
// own private copy of the record shape per §2.5.
const (
	legacyTaskSuffix  = ".meta.json"
	legacyManagerFile = "manager.json"
	migratedDirName   = "state.migrated"
)

// legacyTask mirrors the old state.Task JSON tags so a <id>.meta.json record loads
// verbatim. Carried as a private struct (§2.5) so the importer does not depend on
// internal/state.
type legacyTask struct {
	ID          string    `json:"id"`
	Window      string    `json:"window"`
	Worktree    string    `json:"worktree"`
	Project     string    `json:"project"`
	Harness     string    `json:"harness"`
	Kind        string    `json:"kind"`
	Created     time.Time `json:"created"`
	PR          string    `json:"pr,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	GatePassed  bool      `json:"gatePassed,omitempty"`
	ApprovedBy  string    `json:"approvedBy,omitempty"`
	ReviewedSHA string    `json:"reviewedSha,omitempty"`
	Footprint   []string  `json:"footprint,omitempty"`
}

// legacyManager mirrors the old state.Manager JSON tags (manager.json).
type legacyManager struct {
	Dir       string `json:"dir"`
	SessionID string `json:"sessionId"`
}

// importWarnf surfaces a record the importer could not migrate. Anything that cannot
// be imported is LOGGED, never silently dropped (the source is also preserved under
// state.migrated/ for inspection). It is a package var so a test can capture what was
// surfaced; production writes a one-line note to stderr.
var importWarnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ttorch: legacy import: "+format+"\n", args...)
}

// ImportLegacy is the one-shot, idempotent migration of ttorch's pre-SQLite JSON
// state into the DB (§2.5). It runs at startup after Migrate.
//
// It is a NO-OP unless the DB is pristine (no events AND no tasks, checked with
// COALESCE(MAX(id),0) — never a bare MAX over a possibly-empty table) AND legacy
// files exist under stateDir. So a second run, or a run against an already-populated
// DB, imports nothing.
//
// For each <id>.meta.json it UpsertProjects the task's repo and CreateTasks the row,
// copying the carried fields verbatim; the initial status is active when the task's
// tmux window is live at import time, else torn_down. manager.json becomes the
// singleton manager record and the watch watermark is seeded from the max actionable
// event id. After importing it renames stateDir to a sibling "state.migrated"
// directory (the source is preserved, never deleted — reversible and inspectable),
// which also makes a later run's guard fail.
//
// windowLive reports whether a task's tmux window is currently present (deciding
// active vs torn_down). It is injected rather than calling internal/tmux directly so
// the storage layer keeps no tmux dependency and the liveness branch is
// deterministically testable; a nil predicate treats every task as not-live.
// It returns the number of task records imported.
func ImportLegacy(ctx context.Context, s *Store, stateDir string, windowLive func(window string) bool) (int, error) {
	// Data-loss backstop: never read/rename the real ~/.ttorch/state dir under
	// `go test` (mirrors db.Open). Production is unaffected; a test must point
	// TTORCH_HOME at a temp dir.
	if err := guardRealHomeUnderTest(stateDir); err != nil {
		return 0, err
	}
	// (1a) Are there legacy files to import?
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return 0, nil // no legacy state dir at all
	}
	if err != nil {
		return 0, err
	}
	var metaFiles []string
	hasManager := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == legacyManagerFile {
			hasManager = true
			continue
		}
		if strings.HasSuffix(name, legacyTaskSuffix) {
			metaFiles = append(metaFiles, name)
		}
	}
	if len(metaFiles) == 0 && !hasManager {
		return 0, nil // dir exists but holds no legacy records
	}

	// (1b) Only import into a pristine DB; otherwise this is a re-run or the DB is
	// already populated, so skip without touching the legacy dir (idempotent).
	pristine, err := s.dbPristine(ctx)
	if err != nil {
		return 0, err
	}
	if !pristine {
		return 0, nil
	}

	// (2) Import each task record. os.ReadDir returns entries sorted by name, so the
	// import order is deterministic.
	imported := 0
	for _, name := range metaFiles {
		lt, err := readLegacyTask(filepath.Join(stateDir, name))
		if err != nil {
			// Surface an unloadable record rather than dropping it silently. The file
			// is preserved under state.migrated/ for inspection (step 4).
			importWarnf("skipped %s: cannot parse legacy record: %v", name, err)
			continue
		}
		proj, err := s.UpsertProject(ctx, lt.Project, "")
		if err != nil {
			// A single bad record must not abort the whole migration and strand the rest
			// (the DB would then be non-pristine and a re-run would skip them forever —
			// the silent partial-import the inc1 finding flagged). Surface and continue.
			importWarnf("skipped %s: upsert project %q: %v", name, lt.Project, err)
			continue
		}
		status := StatusTornDown
		if windowLive != nil && windowLive(lt.Window) {
			status = StatusActive
		}
		task := Task{
			ID:          lt.ID,
			ProjectID:   proj.ID,
			Window:      lt.Window,
			Worktree:    lt.Worktree,
			Harness:     lt.Harness,
			Kind:        lt.Kind,
			Created:     lt.Created,
			PR:          lt.PR,
			SessionID:   lt.SessionID,
			GatePassed:  lt.GatePassed,
			ApprovedBy:  lt.ApprovedBy,
			ReviewedSHA: lt.ReviewedSHA,
			Footprint:   lt.Footprint,
			Status:      status,
			// created_by=system records that the import (not a human or the manager)
			// materialized the row, matching the 'created' event's system actor below.
			CreatedBy: ActorSystem,
		}
		// actor=system so the 'created' event records that the import (not a human or
		// the manager) materialized the row (§2.5 step 2).
		if _, err := s.CreateTask(ctx, task, ActorSystem); err != nil {
			importWarnf("skipped %s: create task %q: %v", name, lt.ID, err)
			continue
		}
		imported++
	}

	// (3) Import the manager record and seed the watermark. A bad/unwritable manager
	// record is surfaced (not silently swallowed, as it was before) and the import
	// continues — manager.json is preserved under state.migrated/ and its dir/session
	// are re-derivable, so this never aborts an otherwise-good task migration.
	if hasManager {
		lm, err := readLegacyManager(filepath.Join(stateDir, legacyManagerFile))
		if err != nil {
			importWarnf("skipped %s: cannot parse legacy manager record: %v", legacyManagerFile, err)
		} else if err := s.SetManager(ctx, Manager{Dir: lm.Dir, SessionID: lm.SessionID}); err != nil {
			importWarnf("skipped %s: %v", legacyManagerFile, err)
		}
	}
	maxActionable, err := s.MaxActionableEventID(ctx)
	if err != nil {
		return imported, err
	}
	if err := s.SetWatermark(ctx, maxActionable); err != nil {
		return imported, err
	}

	// (4) Preserve the source: rename (never delete) the legacy dir to a sibling
	// "state.migrated" (reversible + inspectable, §2.5 step 4). This also makes a
	// later run's guard at (1a) fail, so the import never repeats.
	migrated := filepath.Join(filepath.Dir(stateDir), migratedDirName)
	if err := os.Rename(stateDir, migrated); err != nil {
		return imported, fmt.Errorf("import: preserving legacy state as %s: %w", migrated, err)
	}
	return imported, nil
}

// dbPristine reports whether the DB has no events and no tasks — the §2.5 import
// guard. It uses COALESCE(MAX(id),0) (never a bare MAX, which is NULL on an empty
// table) for the events probe.
func (s *Store) dbPristine(ctx context.Context) (bool, error) {
	var maxEvent int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events`).Scan(&maxEvent); err != nil {
		return false, err
	}
	if maxEvent != 0 {
		return false, nil
	}
	var taskCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks`).Scan(&taskCount); err != nil {
		return false, err
	}
	return taskCount == 0, nil
}

// readLegacyTask parses one <id>.meta.json record.
func readLegacyTask(path string) (legacyTask, error) {
	var lt legacyTask
	b, err := os.ReadFile(path)
	if err != nil {
		return lt, err
	}
	if err := json.Unmarshal(b, &lt); err != nil {
		return lt, err
	}
	return lt, nil
}

// readLegacyManager parses manager.json.
func readLegacyManager(path string) (legacyManager, error) {
	var lm legacyManager
	b, err := os.ReadFile(path)
	if err != nil {
		return lm, err
	}
	if err := json.Unmarshal(b, &lm); err != nil {
		return lm, err
	}
	return lm, nil
}
