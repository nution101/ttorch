package orchestrator

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func (m *Manager) audit(line string) { _ = m.writeAudit(line) }

// sanitizeAuditLine renders a record on exactly one line, escaping the C0 control characters
// and DEL that would otherwise break the log's framing.
//
// The log is newline-delimited, and callers interpolate values they did not author — a
// changed filename, a task id, a git ref, a refusal reason. A value containing a newline
// therefore writes a SECOND record that is syntactically indistinguishable from a real one:
// with a filename alone a worker could forge a complete "merge-local ... approver=human"
// entry for a task that never merged. Escaping belongs here, at the sink, because every
// caller shares the exposure and no caller can be relied on to remember.
//
// The guard also refuses a changed path carrying a control character before a merge can
// reach this point (orchestrator.hostilePath). That closes the known route; this closes the
// class, for the call sites nobody has audited yet.
//
// C1 (U+0080–U+009F) is escaped alongside C0 and DEL: U+0085 NEL is a line break, and U+009B
// is CSI, the single-character form of "ESC [" that several terminals accept — so a log a
// human greps or pipes to a terminal must not carry them raw either.
func isAuditControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }

func sanitizeAuditLine(line string) string {
	if strings.IndexFunc(line, isAuditControl) < 0 {
		return line
	}
	var b strings.Builder
	b.Grow(len(line) + 8)
	for _, r := range line {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case isAuditControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeAudit appends a timestamped record to the audit log and flushes it to disk,
// returning any error. Trusted merges call it directly and ABORT on failure — an
// unrecorded finance merge is not acceptable (every trusted merge must be
// reconstructable). Other call sites use audit() best-effort.
//
// Every record is escaped through sanitizeAuditLine first, so ONE call produces exactly ONE
// line whatever it was handed.
func (m *Manager) writeAudit(line string) error {
	if err := os.MkdirAll(m.P.Home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.P.AuditLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), sanitizeAuditLine(line)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
