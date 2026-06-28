package orchestrator

import (
	"fmt"
	"os"
	"time"
)

func (m *Manager) audit(line string) { _ = m.writeAudit(line) }

// writeAudit appends a timestamped record to the audit log and flushes it to disk,
// returning any error. Trusted merges call it directly and ABORT on failure — an
// unrecorded finance merge is not acceptable (every trusted merge must be
// reconstructable). Other call sites use audit() best-effort.
func (m *Manager) writeAudit(line string) error {
	if err := os.MkdirAll(m.P.Home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.P.AuditLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
