package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTakeZFSCmdPlaceholders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	out := filepath.Join(t.TempDir(), "out")
	o := &esxiOpts{zfsCmd: "echo '{dataset}|{snapshot}|{fullname}' > " + out}
	if err := takeZFS(o, "tank/nfs", "auto_1", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.TrimSpace(string(b)) != "tank/nfs|auto_1|tank/nfs@auto_1" {
		t.Errorf("got %q", b)
	}
}

func TestTakeZFSCmdRefusesUnsafeNames(t *testing.T) {
	o := &esxiOpts{zfsCmd: "true {fullname}"}
	for _, bad := range []string{"a;rm -rf /", "a$(id)", "a b", "`x`", "a|b", ""} {
		if err := takeZFS(o, "tank/nfs", bad, true); err == nil {
			t.Errorf("snapshot name %q must be refused", bad)
		}
		if err := takeZFS(o, bad, "s", true); err == nil {
			t.Errorf("dataset %q must be refused", bad)
		}
	}
}

func TestTakeZFSCmdFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	o := &esxiOpts{zfsCmd: "echo boom >&2; exit 3"}
	err := takeZFS(o, "tank/nfs", "s", true)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("want failure with output, got %v", err)
	}
}

func TestEsxiTimeout(t *testing.T) {
	if esxiTimeout(30*time.Second, false) != 120*time.Second {
		t.Error("unset --timeout should default to 120s for ESXi")
	}
	if esxiTimeout(5*time.Second, true) != 5*time.Second {
		t.Error("explicit --timeout must win")
	}
}

func TestNfsPathFor(t *testing.T) {
	o := &esxiOpts{nfsPath: "/x/y"}
	if p, w := o.nfsPathFor("tank/nfs"); p != "/x/y" || w != "" {
		t.Errorf("explicit path: %q %q", p, w)
	}
	o = &esxiOpts{}
	if _, w := o.nfsPathFor(""); w == "" {
		t.Error("no path and no dataset must warn")
	}
}
