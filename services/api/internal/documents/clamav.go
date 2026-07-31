package documents

// ClamAV malware scan on upload (doc 06 §5) — accepting arbitrary file uploads
// from users without a scan is a real risk. Uses clamdscan against the local
// clamd daemon; if the daemon is down the upload is REJECTED (fail closed — we
// don't want an unscanned file entering the pipeline).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// errInfected is returned when the scan finds malware.
var errInfected = fmt.Errorf("file failed malware scan")

// scanWithClamAV runs clamdscan over the given bytes. Returns nil if clean,
// errInfected if malware is found, or a wrapped error if the scan itself fails.
func scanWithClamAV(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil
	}

	// clamdscan needs a path; write to a temp file then scan. --stream forces
	// streaming to clamd so the temp file never persists on disk.
	tmp, err := os.CreateTemp("", "scan-*")
	if err != nil {
		return fmt.Errorf("create temp for scan: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp for scan: %w", err)
	}
	tmp.Close()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Exit codes: 0 = clean, 1 = infected, 2+ = error.
	cmd := exec.CommandContext(cctx, "clamdscan", "--stream", "--no-summary", tmpName)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil // clean
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		switch exitErr.ExitCode() {
		case 1:
			slog.Warn("malware detected on upload", "detail", strings.TrimSpace(string(out)))
			return errInfected
		case 2, 126, 127:
			// clamd unreachable (126=perm, 127=not found) — fail closed.
			return fmt.Errorf("clamd scan unavailable (exit %d): %s",
				exitErr.ExitCode(), strings.TrimSpace(string(out)))
		default:
			return fmt.Errorf("clamd scan error (exit %d): %s",
				exitErr.ExitCode(), strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("clamd scan failed: %w", err)
}
