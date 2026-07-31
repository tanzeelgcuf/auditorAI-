package documents

import (
	"context"
	"os/exec"
	"testing"
)

// EICAR test string — the standard AV test signature (harmless, detected by ClamAV).
const eicar = "X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"

// TestScanClean verifies a benign file passes. Skips if clamd isn't running.
func TestScanClean(t *testing.T) {
	if _, err := exec.LookPath("clamdscan"); err != nil {
		t.Skip("clamdscan not installed")
	}
	if err := scanWithClamAV(context.Background(), []byte("clean,test,content\n")); err != nil {
		t.Fatalf("clean file should pass scan, got %v", err)
	}
}

// TestScanInfected verifies the EICAR signature is rejected. Skips if clamd is down.
func TestScanInfected(t *testing.T) {
	if _, err := exec.LookPath("clamdscan"); err != nil {
		t.Skip("clamdscan not installed")
	}
	if err := scanWithClamAV(context.Background(), []byte(eicar)); err == nil {
		t.Fatal("EICAR signature should be flagged as infected")
	}
}

// TestScanEmpty is a no-op (nothing to scan).
func TestScanEmpty(t *testing.T) {
	if err := scanWithClamAV(context.Background(), nil); err != nil {
		t.Fatalf("empty input should pass, got %v", err)
	}
	if err := scanWithClamAV(context.Background(), []byte{}); err != nil {
		t.Fatalf("empty input should pass, got %v", err)
	}
}
