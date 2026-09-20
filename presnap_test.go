package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunPreSnap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	if w := runPreSnap("true", 5*time.Second); w != "" {
		t.Errorf("ok command: %q", w)
	}
	if w := runPreSnap("echo boom >&2; exit 3", 5*time.Second); !strings.Contains(w, "pre-snap-cmd failed") || !strings.Contains(w, "boom") {
		t.Errorf("failing command: %q", w)
	}
	if w := runPreSnap("sleep 5", 200*time.Millisecond); !strings.Contains(w, "timed out") {
		t.Errorf("timeout: %q", w)
	}
}
