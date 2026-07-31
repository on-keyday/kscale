package remoteexec

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// auditDir, when non-empty, is the directory remote-shell session transcripts are
// written to. Read once from KSCALE_SHELL_AUDIT_DIR. Empty => no transcript files;
// only the Start/Exit summary lines (who / what command / when) are logged.
var auditDir = os.Getenv("KSCALE_SHELL_AUDIT_DIR")

// LogAuditor implements exec.Auditor (objtrsf/exec): it restores the remote-shell
// audit trail that the exec extraction dropped. Start/Exit are logged (the
// authz-relevant who/what/when); when an audit dir is configured the full session
// transcript (stdin keystrokes + stdout/stderr) is recorded to a per-session file.
// Thread-safe — the exec taps fire from separate stdout/stderr/stdin goroutines.
type LogAuditor struct {
	logger *slog.Logger
	label  string
	mu     sync.Mutex
	w      io.WriteCloser // per-session transcript; nil => summary-only
}

// NewLogAuditor builds an auditor for one session. label identifies the caller /
// target (e.g. `cplane cn=admin.…` or `dp cmd=sh`). A transcript file is opened
// under KSCALE_SHELL_AUDIT_DIR when set; otherwise the session is summary-only.
func NewLogAuditor(logger *slog.Logger, label string) *LogAuditor {
	a := &LogAuditor{logger: logger, label: label}
	if auditDir == "" {
		return a
	}
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		logger.Warn("remote-shell audit: mkdir", "dir", auditDir, "error", err)
		return a
	}
	name := time.Now().UTC().Format("20060102T150405.000Z") + "_" + sanitizeLabel(a.label) + ".log"
	f, err := os.OpenFile(filepath.Join(auditDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logger.Warn("remote-shell audit: open transcript", "error", err)
		return a
	}
	a.w = f
	return a
}

func (a *LogAuditor) Start(command string, args []string, ptyEnabled bool) {
	a.logger.Info("remote-shell session start", "session", a.label, "command", command, "args", args, "pty", ptyEnabled)
	a.line("== start %s command=%q args=%v pty=%v ==", time.Now().UTC().Format(time.RFC3339), command, args, ptyEnabled)
}

func (a *LogAuditor) Stdin(data []byte)  { a.chunk("IN ", data) }
func (a *LogAuditor) Stdout(data []byte) { a.chunk("OUT", data) }
func (a *LogAuditor) Stderr(data []byte) { a.chunk("ERR", data) }

func (a *LogAuditor) Exit(err error) {
	a.logger.Info("remote-shell session end", "session", a.label, "error", errString(err))
	a.line("== exit %s error=%v ==", time.Now().UTC().Format(time.RFC3339), err)
	a.mu.Lock()
	if a.w != nil {
		_ = a.w.Close()
		a.w = nil
	}
	a.mu.Unlock()
}

func (a *LogAuditor) line(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.w == nil {
		return
	}
	fmt.Fprintf(a.w, format+"\n", args...)
}

// chunk records one stdin/stdout/stderr fragment verbatim (quoted so control /
// escape bytes stay legible). Called synchronously under the lock, so it never
// retains data beyond the write.
func (a *LogAuditor) chunk(dir string, data []byte) {
	if len(data) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.w == nil {
		return
	}
	fmt.Fprintf(a.w, "%s %s %q\n", time.Now().UTC().Format("15:04:05.000"), dir, string(data))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sanitizeLabel keeps a label safe for a filename (alnum, dot, dash, underscore).
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 96 {
		out = out[:96]
	}
	return out
}
