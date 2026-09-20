package freezer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeQM is a shell script that plays qm: "snapshot" appends a snapshot section
// to <FAKE_QM_DIR>/<vmid>.conf the way Proxmox writes it (description as an
// URL-escaped #comment), "delsnapshot" removes it. FAKE_QM_LOG records the calls,
// FAKE_QM_SLEEP delays a snapshot, FAKE_QM_FAIL makes it fail.
const fakeQM = `#!/bin/sh
echo "$@" >> "$FAKE_QM_LOG"
case "$1" in
snapshot)
  vmid=$2; name=$3; desc=""
  while [ $# -gt 0 ]; do [ "$1" = "--description" ] && desc=$2; shift; done
  [ -n "$FAKE_QM_SLEEP" ] && sleep "$FAKE_QM_SLEEP"
  if [ -n "$FAKE_QM_FAIL" ]; then echo "snapshot failed: no space" >&2; exit 255; fi
  enc=$(printf %s "$desc" | sed 's/ /%20/g')
  printf '\n[%s]\n#%s\nsnaptime: %s\nvmstate: local-zfs:vm-%s-state-%s\n' "$name" "$enc" "${FAKE_QM_TIME:-$(date +%s)}" "$vmid" "$name" >> "$FAKE_QM_DIR/$vmid.conf"
  ;;
delsnapshot)
  awk -v n="[$3]" '/^\[/{skip=($0==n)} !skip{print}' "$FAKE_QM_DIR/$2.conf" > "$FAKE_QM_DIR/$2.conf.new" && mv "$FAKE_QM_DIR/$2.conf.new" "$FAKE_QM_DIR/$2.conf"
  ;;
esac
`

type qmEnv struct{ dir, log string }

func setupQM(t *testing.T) qmEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake qm is a shell script")
	}
	dir := t.TempDir()
	e := qmEnv{dir: dir, log: filepath.Join(dir, "qm.log")}
	bin := filepath.Join(dir, "qm")
	if err := os.WriteFile(bin, []byte(fakeQM), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "100.conf"), []byte("boot: order=scsi0\nmemory: 2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envQM, bin)
	t.Setenv(envPVEConfDir, dir)
	t.Setenv("FAKE_QM_DIR", dir)
	t.Setenv("FAKE_QM_LOG", e.log)
	SetSnapName("")
	t.Cleanup(func() { SetSnapName(""); SetPolicy(nil); zfsSnapExists = realZFSSnapExists; nowUnix = realNowUnix })
	return e
}

var (
	realZFSSnapExists = zfsSnapExists
	realNowUnix       = nowUnix
)

func (e qmEnv) calls() string { b, _ := os.ReadFile(e.log); return string(b) }

func (e qmEnv) snaps(t *testing.T, vmid int) []string {
	t.Helper()
	ss, err := readPVESnaps(vmid)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, s := range ss {
		n = append(n, s.Name)
	}
	return n
}

// a running VM has a qmp socket; a plain file is enough for os.Stat
func qmpFile(t *testing.T, vmid int) *QEMUFreezer {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "100.qmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &QEMUFreezer{SocketDir: d, QMPSocketDir: d, strategies: map[int]string{}, memSnaps: map[int]string{}, notes: map[int]string{}}
}

func TestPVESnapName(t *testing.T) {
	for in, want := range map[string]string{
		"daily-1669998751_2026.09.12": "cs4s_daily_1669998751_2026_09_12",
		"":                            "cs4s_snap",
	} {
		if got := pveSnapName(in); got != want {
			t.Errorf("pveSnapName(%q) = %q, want %q", in, got, want)
		}
	}
	long := pveSnapName("1789858862_repli_zfs_127.0.0.1_nr_6_and_a_very_long_tail_nr_77")
	if len(long) != 40 || !strings.HasSuffix(long, "nr_77") || !strings.HasPrefix(long, PVESnapPrefix) {
		t.Errorf("long name: %q (%d)", long, len(long))
	}
}

func TestParsePVEConfSnaps(t *testing.T) {
	conf := "boot: order=scsi0\nmemory: 2048\n\n[cs4s_a]\n#cs-freeze4snap%20job_nr_1\nsnaptime: 1700000000\nsnapstate: prepare\n\n[other]\ndescription: hand made\nsnaptime: 1700000100\n"
	ss := parsePVEConfSnaps(conf)
	if len(ss) != 2 || ss[0].Name != "cs4s_a" || ss[0].Time != 1700000000 || ss[0].State != "prepare" || !ss[0].tagged() || ss[0].zfsName() != "job_nr_1" {
		t.Fatalf("snaps: %+v", ss)
	}
	if ss[1].tagged() || ss[1].Desc != "hand made" {
		t.Errorf("foreign snapshot: %+v", ss[1])
	}
}

func TestQMMemoryStepKeepsSnapshot(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[memory,zfs]\n", "")
	SetSnapName("snapA.nr_1")
	q := qmpFile(t, 100)
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-0"}

	s, err := q.Freeze(g, time.Second)
	if s != StrategyQMMem || err != nil {
		t.Fatalf("freeze: %q %v", s, err)
	}
	if !strings.Contains(e.calls(), "snapshot 100 cs4s_snapA_nr_1 --vmstate 1 --description cs-freeze4snap snapA.nr_1") {
		t.Errorf("qm calls: %s", e.calls())
	}
	// Thaw must not delete the snapshot: it is the restore point of the ZFS snapshot
	zfsSnapExists = func(ds, sn string) (bool, bool) { return true, true }
	if err := q.Thaw(g); err != nil {
		t.Fatal(err)
	}
	if got := e.snaps(t, 100); len(got) != 1 || got[0] != "cs4s_snapA_nr_1" {
		t.Errorf("snapshots after thaw: %v", got)
	}
	if strings.Contains(e.calls(), "delsnapshot") {
		t.Errorf("thaw removed something: %s", e.calls())
	}
}

func TestQMMemoryFollowsZFSSnapshot(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[memory,zfs]\n", "")
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-0"}
	old := time.Now().Add(-time.Hour).Unix()
	exists := map[string]bool{"r1": true, "r2": true, "r3": true}
	zfsSnapExists = func(ds, sn string) (bool, bool) { return exists[sn], true }

	// three runs, an hour apart
	for i, n := range []string{"r1", "r2", "r3"} {
		t.Setenv("FAKE_QM_TIME", itoa(old+int64(i)*10))
		SetSnapName(n)
		q := qmpFile(t, 100)
		if s, err := q.Freeze(g, time.Second); s != StrategyQMMem || err != nil {
			t.Fatalf("run %s: %q %v", n, s, err)
		}
		_ = q.Thaw(g)
	}
	if got := e.snaps(t, 100); len(got) != 3 {
		t.Fatalf("all three ZFS snapshots exist, all three memory snapshots stay: %v", got)
	}
	// retention destroys the ZFS snapshot r1: its memory snapshot goes at the next run
	exists["r1"] = false
	t.Setenv("FAKE_QM_TIME", itoa(time.Now().Unix()))
	SetSnapName("r4")
	exists["r4"] = true
	q := qmpFile(t, 100)
	if s, err := q.Freeze(g, time.Second); s != StrategyQMMem || err != nil {
		t.Fatal(s, err)
	}
	_ = q.Thaw(g)
	got := strings.Join(e.snaps(t, 100), ",")
	if got != "cs4s_r2,cs4s_r3,cs4s_r4" {
		t.Errorf("after retention of r1: %s", got)
	}
}

func TestQMMemoryOrphanGrace(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[zfs]\n", "") // no memory step this run, only the prune at the start
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-0"}
	zfsSnapExists = func(ds, sn string) (bool, bool) { return false, true }
	// a snapshot of a parallel job that has no ZFS snapshot yet, and an old orphan
	t.Setenv("FAKE_QM_TIME", itoa(time.Now().Unix()))
	if _, err := runQMForTest(t, "snapshot", "100", "cs4s_young", "--description", "cs-freeze4snap young"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_QM_TIME", itoa(time.Now().Add(-time.Hour).Unix()))
	if _, err := runQMForTest(t, "snapshot", "100", "cs4s_old", "--description", "cs-freeze4snap old"); err != nil {
		t.Fatal(err)
	}
	// a foreign snapshot with the prefix but without the tag stays whatever happens
	t.Setenv("FAKE_QM_TIME", itoa(time.Now().Add(-time.Hour).Unix()))
	if _, err := runQMForTest(t, "snapshot", "100", "cs4s_mine", "--description", "hand made"); err != nil {
		t.Fatal(err)
	}
	q := qmpFile(t, 100)
	if s, err := q.Freeze(g, time.Second); s != StrategyZFSOnly || err != nil {
		t.Fatal(s, err)
	}
	if got := strings.Join(e.snaps(t, 100), ","); got != "cs4s_young,cs4s_mine" {
		t.Errorf("snapshots: %s", got)
	}
}

// After "zfs rollback -r <disk>@cs4s_x" (the restore) the ZFS snapshot is gone from the
// disk zvol but still on the vmstate zvol: the memory snapshot in use must stay.
func TestQMMemoryKeptWhileVMStateZvolHasSnapshot(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[zfs]\n", "")
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-0"}
	var asked []string
	zfsSnapExists = func(ds, sn string) (bool, bool) {
		asked = append(asked, ds)
		return ds == "rpool/data/vm-100-state-cs4s_old", true
	}
	t.Setenv("FAKE_QM_TIME", itoa(time.Now().Add(-time.Hour).Unix()))
	if _, err := runQMForTest(t, "snapshot", "100", "cs4s_old", "--description", "cs-freeze4snap old"); err != nil {
		t.Fatal(err)
	}
	q := qmpFile(t, 100)
	if _, err := q.Freeze(g, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.snaps(t, 100), ","); got != "cs4s_old" {
		t.Errorf("snapshot in use was removed: %q (asked %v)", got, asked)
	}
}

func TestQMMemkeepCap(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[memory,zfs]\nmemkeep=2\n", "")
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-0"}
	zfsSnapExists = func(ds, sn string) (bool, bool) { return true, true } // retention keeps everything
	old := time.Now().Add(-time.Hour).Unix()
	for i, n := range []string{"r1", "r2", "r3"} {
		t.Setenv("FAKE_QM_TIME", itoa(old+int64(i)*10))
		SetSnapName(n)
		q := qmpFile(t, 100)
		if s, err := q.Freeze(g, time.Second); s != StrategyQMMem || err != nil {
			t.Fatal(s, err)
		}
		if err := q.Thaw(g); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(e.snaps(t, 100), ","); got != "cs4s_r2,cs4s_r3" {
		t.Errorf("memkeep=2: %s", got)
	}
}

func TestQMMemoryFallbackNoteAndFailure(t *testing.T) {
	e := setupQM(t)
	withPolicy(t, "[quiesce,memory,zfs]\n", "")
	SetSnapName("n1")
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU}
	q := qmpFile(t, 100)
	// freeze fails (no qga socket) -> memory takes over, with a note
	if s, err := q.Freeze(g, time.Second); s != StrategyQMMem || err != nil {
		t.Fatal(s, err)
	}
	if n := q.Note(g); !strings.Contains(n, "freeze:") || !strings.Contains(n, "took a memory snapshot instead") || !strings.Contains(n, "qm rollback 100 cs4s_n1") {
		t.Errorf("note: %q", n)
	}
	// qm snapshot fails: chain goes on, no leftover
	t.Setenv("FAKE_QM_FAIL", "1")
	SetSnapName("n2")
	q2 := qmpFile(t, 100)
	withPolicy(t, "[memory]\n", "") // strict
	s, err := q2.Freeze(g, time.Second)
	if err != nil || s != StrategyNone {
		t.Fatalf("failed memory step: %q %v", s, err)
	}
	if got := strings.Join(e.snaps(t, 100), ","); got != "cs4s_n1" {
		t.Errorf("snapshots: %s", got)
	}
}

func TestQMMemoryTimeout(t *testing.T) {
	setupQM(t)
	withPolicy(t, "[memory:1,zfs]\n", "")
	t.Setenv("FAKE_QM_SLEEP", "10")
	SetSnapName("slow")
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU}
	q := qmpFile(t, 100)
	start := time.Now()
	s, err := q.Freeze(g, time.Second)
	if err != nil || s != StrategyNone {
		t.Fatalf("%q %v", s, err)
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
}

func TestMemkeepPolicyLines(t *testing.T) {
	p, err := ParsePolicy("t", "memkeep=5\n192.168.2.112:memkeep=2\n192.168.2.113:memkeep=9\n[memory,zfs]\n", "192.168.2.112")
	if err != nil {
		t.Fatal(err)
	}
	if p.MemKeep() != 2 {
		t.Errorf("server line wins: %d", p.MemKeep())
	}
	q, _ := ParsePolicy("t", "memkeep=5\n192.168.2.112:memkeep=2\n", "10.0.0.1")
	if q.MemKeep() != 5 {
		t.Errorf("global: %d", q.MemKeep())
	}
	f, err := PolicyOfFlags([]string{"memkeep=3", "[memory,zfs]"})
	if err != nil || f.MemKeep() != 3 {
		t.Errorf("flag: %v %v", f, err)
	}
	if m := p.Merge(f); m.MemKeep() != 3 {
		t.Errorf("flags beat the cfg: %d", m.MemKeep())
	}
	for _, bad := range []string{"memkeep=x", "memkeep=-1", "memkeep=1000"} {
		if _, err := ParsePolicy("t", bad+"\n", ""); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !IsPolicyLine("memkeep=3") || !IsPolicyLine("192.168.2.112:memkeep=3") {
		t.Error("memkeep lines must be policy lines")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func runQMForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runQM(ctx, args...)
}
