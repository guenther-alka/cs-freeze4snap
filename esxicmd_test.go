package main

import (
	"cs-freeze4snap/freezer"
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

func TestLoadPolicyPrecedence(t *testing.T) {
	defer freezer.SetPolicy(nil)
	cfg := filepath.Join(t.TempDir(), "f.cfg")
	os.WriteFile(cfg, []byte("192.168.2.48,root,pw\n[quiesce,memory,zfs,30]\n192.168.2.48:vm100,plain,zfs\n"), 0o600)
	o := &esxiOpts{cfg: cfg}
	if err := o.loadPolicy("192.168.2.48"); err != nil {
		t.Fatal(err)
	}
	g := func(id int) string {
		return freezer.ChainFor(freezer.Guest{VMID: id, Platform: freezer.PlatformESXi}, freezer.DefaultESXiChain).String()
	}
	if g(100) != "plain,zfs" || g(101) != "freeze,memory,zfs,30" {
		t.Errorf("cfg: %s / %s", g(100), g(101))
	}
	o.policy = multiFlag{"vm100,memory,zfs"}
	if err := o.loadPolicy("192.168.2.48"); err != nil || g(100) != "memory,zfs" {
		t.Errorf("--policy must beat the cfg: %s %v", g(100), err)
	}
	o.mode = "plain"
	if err := o.loadPolicy("192.168.2.48"); err != nil || g(100) != "plain,zfs" || g(5) != "plain,zfs" {
		t.Errorf("--mode must beat everything: %s %s %v", g(100), g(5), err)
	}
	o.mode = "bogus"
	if o.loadPolicy("192.168.2.48") == nil {
		t.Error("bad --mode accepted")
	}
	o = &esxiOpts{policy: multiFlag{"vm1,nonsense"}}
	if o.loadPolicy("") == nil {
		t.Error("bad --policy accepted")
	}
}

func TestAbortList(t *testing.T) {
	rs := []freezer.GuestResult{
		{VMID: 1, Frozen: true, Strict: true},
		{VMID: 2, Strict: true},                                    // strict, not frozen
		{VMID: 3, Strict: false},                                   // best effort
		{VMID: 4, Strict: true, Strategy: freezer.StrategyZFSOnly}, // asked for zfs only
	}
	if got := abortList(rs, false); len(got) != 1 || got[0] != 2 {
		t.Errorf("strict: %v", got)
	}
	if got := abortList(rs, true); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("forcefreeze: %v", got)
	}
}
