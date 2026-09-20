package freezer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cs-freeze4snap/esxi"
)

// fakeTr is an in-memory ESXi transport.
type fakeTr struct {
	mu       sync.Mutex
	snaps    map[int][]esxi.Snapshot
	next     int
	quiesceF map[int]bool // quiesce requests for these VMs fail
	allFail  map[int]bool // every create for these VMs fails
	hang     map[int]bool // create blocks until the context ends
	calls    []string
	rmFail   map[int]bool
}

func newFakeTr() *fakeTr {
	return &fakeTr{snaps: map[int][]esxi.Snapshot{}, quiesceF: map[int]bool{}, allFail: map[int]bool{}, hang: map[int]bool{}, rmFail: map[int]bool{}}
}

func (f *fakeTr) Name() string                                         { return "fake" }
func (f *fakeTr) Datastores(context.Context) ([]esxi.Datastore, error) { return nil, nil }
func (f *fakeTr) VMs(context.Context) ([]esxi.VM, error)               { return nil, nil }
func (f *fakeTr) Warnings() []string                                   { return nil }
func (f *fakeTr) Close() error                                         { return nil }
func (f *fakeTr) Snapshots(_ context.Context, id int) ([]esxi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]esxi.Snapshot(nil), f.snaps[id]...), nil
}

func (f *fakeTr) CreateSnapshot(ctx context.Context, id int, name, desc string, mem, quiesce bool) (string, error) {
	if f.hang[id] {
		<-ctx.Done()
		return "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("create %d mem=%t q=%t", id, mem, quiesce))
	if f.allFail[id] || (quiesce && f.quiesceF[id]) {
		return "", errors.New("VMware Tools is not running")
	}
	f.next++
	sid := fmt.Sprint(f.next)
	f.snaps[id] = append(f.snaps[id], esxi.Snapshot{ID: sid, Name: name, Desc: desc})
	return sid, nil
}

func (f *fakeTr) RemoveSnapshot(_ context.Context, id int, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("remove %d %s", id, sid))
	if f.rmFail[id] {
		return errors.New("host busy")
	}
	var keep []esxi.Snapshot
	for _, s := range f.snaps[id] {
		if s.ID != sid {
			keep = append(keep, s)
		}
	}
	if len(keep) == len(f.snaps[id]) {
		return errors.New("VM has no such snapshot")
	}
	f.snaps[id] = keep
	return nil
}

func esxiGuests(ids ...int) []Guest {
	var gs []Guest
	for _, id := range ids {
		gs = append(gs, Guest{VMID: id, Name: fmt.Sprintf("vm%d", id), Type: TypeVM, Platform: PlatformESXi})
	}
	return gs
}

func TestESXiFreezeThaw(t *testing.T) {
	tr := newFakeTr()
	f, err := NewESXiFreezer(tr, "", "daily-123_2026.09.19")
	if err != nil {
		t.Fatal(err)
	}
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	res := sess.Results()
	if len(res) != 2 {
		t.Fatalf("results: %+v", res)
	}
	for _, r := range res {
		if !r.Frozen || r.Strategy != StrategyESXiQuiesce || r.Warning != "" || r.SnapID == "" || r.Name == "" {
			t.Errorf("guest result: %+v", r)
		}
	}
	for _, id := range []int{10, 11} {
		s := tr.snaps[id]
		if len(s) != 1 || s[0].Desc != ESXiTag || s[0].Name != fmt.Sprintf("cs4s-daily-123_2026.09.19-%d", id) {
			t.Errorf("vm %d snapshots: %+v", id, s)
		}
	}
	sess.Thaw()
	for _, r := range sess.Results() {
		if !r.Thawed {
			t.Errorf("not thawed: %+v", r)
		}
	}
	if len(tr.snaps[10])+len(tr.snaps[11]) != 0 {
		t.Errorf("VM snapshots left behind: %+v", tr.snaps)
	}
	sess.Thaw() // idempotent
}

func TestESXiQuiesceFallback(t *testing.T) {
	tr := newFakeTr()
	tr.quiesceF[10] = true
	f, _ := NewESXiFreezer(tr, ModeQuiesce, "s")
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	defer sess.Thaw()
	byID := map[int]GuestResult{}
	for _, r := range sess.Results() {
		byID[r.VMID] = r
	}
	if r := byID[10]; !r.Frozen || r.Strategy != StrategyESXiSnap || !strings.Contains(r.Warning, "crash-consistent") || !strings.Contains(r.Warning, "Tools") {
		t.Errorf("fallback result: %+v", r)
	}
	if r := byID[11]; r.Strategy != StrategyESXiQuiesce || r.Warning != "" {
		t.Errorf("healthy vm: %+v", r)
	}
}

func TestESXiAllFail(t *testing.T) {
	tr := newFakeTr()
	tr.allFail[10] = true
	f, _ := NewESXiFreezer(tr, ModeMem, "s")
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10, 12), 5*time.Second)
	sess.Thaw()
	for _, r := range sess.Results() {
		switch r.VMID {
		case 10:
			if r.Frozen || r.Warning == "" || r.SnapID != "" {
				t.Errorf("failed guest: %+v", r)
			}
		case 12:
			if !r.Frozen || r.Strategy != StrategyESXiMem || !r.Thawed {
				t.Errorf("mem guest: %+v", r)
			}
		}
	}
	for _, c := range tr.calls {
		if strings.HasPrefix(c, "remove 10") {
			t.Errorf("thaw removed a snapshot that was never created: %v", tr.calls)
		}
	}
}

func TestESXiTimeout(t *testing.T) {
	tr := newFakeTr()
	tr.hang[10] = true
	f, _ := NewESXiFreezer(tr, ModePlain, "s")
	Register(f)
	defer unregisterLast()

	start := time.Now()
	sess := FreezeAll(esxiGuests(10, 11), 300*time.Millisecond)
	defer sess.Thaw()
	if time.Since(start) > 3*time.Second {
		t.Errorf("hanging vm blocked the freeze for %v", time.Since(start))
	}
	for _, r := range sess.Results() {
		if r.VMID == 10 && (r.Frozen || r.Warning == "") {
			t.Errorf("hanging vm: %+v", r)
		}
		if r.VMID == 11 && !r.Frozen {
			t.Errorf("healthy vm was blocked: %+v", r)
		}
	}
}

func TestESXiThawFailureIsReported(t *testing.T) {
	tr := newFakeTr()
	tr.rmFail[10] = true
	f, _ := NewESXiFreezer(tr, ModePlain, "s")
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10), 5*time.Second)
	sess.Thaw()
	r := sess.Results()[0]
	if r.Thawed || !strings.Contains(r.Warning, "cleanup") {
		t.Errorf("thaw failure must be visible: %+v", r)
	}
}

func TestESXiStateRoundTrip(t *testing.T) {
	tr := newFakeTr()
	f, _ := NewESXiFreezer(tr, ModePlain, "snap")
	Register(f) // removed explicitly below

	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	st := ESXiState{Host: "h", Dataset: "tank/nfs", Snapshot: "snap", Guests: f.State()}
	p := filepath.Join(t.TempDir(), "st.json")
	if err := WriteESXiState(p, st); err != nil {
		t.Fatal(err)
	}
	// Windows has no unix permission bits (Perm() reports 0666), so the mode is only checked elsewhere
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v", fi.Mode().Perm())
	}
	_ = sess // the freezing process "dies" here without thawing

	// a new process reads the state and thaws
	back, err := ReadESXiState(p)
	if err != nil || len(back.Guests) != 2 {
		t.Fatalf("read: %+v %v", back, err)
	}
	unregisterLast() // the first process is gone
	f2, _ := NewESXiFreezer(tr, ModePlain, "snap")
	f2.Restore(back.Guests)
	Register(f2)
	defer unregisterLast()

	var gs []Guest
	for _, g := range back.Guests {
		gs = append(gs, Guest{VMID: g.VMID, Platform: PlatformESXi, Type: TypeVM})
	}
	RestoreSession(gs).Thaw()
	if len(tr.snaps[10])+len(tr.snaps[11]) != 0 {
		t.Errorf("snapshots left after thaw from state: %+v", tr.snaps)
	}
	if _, err := ReadESXiState(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing state file must be an error")
	}
}

func TestCleanupESXi(t *testing.T) {
	tr := newFakeTr()
	tr.snaps[10] = []esxi.Snapshot{
		{ID: "1", Name: "manual", Desc: ""},
		{ID: "2", Name: "cs4s-old-10", Desc: ESXiTag},
		{ID: "3", Name: "cs4s-lookalike", Desc: "mine"}, // right prefix, wrong tag
		{ID: "4", Name: "nightly", Desc: ESXiTag},       // right tag, wrong name
	}
	removed, errs := CleanupESXi(context.Background(), tr, []esxi.VM{{ID: 10, Name: "a"}})
	if len(removed) != 1 || len(errs) != 0 || !strings.Contains(removed[0], "cs4s-old-10") {
		t.Fatalf("removed %v errs %v", removed, errs)
	}
	if len(tr.snaps[10]) != 3 {
		t.Errorf("only the tagged snapshot may go: %+v", tr.snaps[10])
	}
}

func TestESXiNameAndMode(t *testing.T) {
	f, _ := NewESXiFreezer(newFakeTr(), "", "we'ird name; rm -rf /")
	n := f.vmSnapName(Guest{VMID: 7})
	if strings.ContainsAny(n, " ;'/") || !strings.HasSuffix(n, "-7") {
		t.Errorf("unsafe snapshot name %q", n)
	}
	if _, err := NewESXiFreezer(newFakeTr(), "bogus", "s"); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestESXiThawWhenSnapshotIsAlreadyGone(t *testing.T) {
	tr := newFakeTr()
	f, _ := NewESXiFreezer(tr, ModePlain, "s")
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10), 5*time.Second)
	tr.snaps[10] = nil // cleanup (or the admin) removed it meanwhile
	sess.Thaw()
	r := sess.Results()[0]
	if !r.Thawed || r.Warning != "" {
		t.Errorf("a vanished snapshot means thawed, not an error: %+v", r)
	}
}

func TestCleanupESXiManyVMsKeepsOrder(t *testing.T) {
	tr := newFakeTr()
	var vms []esxi.VM
	for id := 1; id <= 20; id++ {
		vms = append(vms, esxi.VM{ID: id, Name: fmt.Sprintf("vm%d", id)})
		tr.snaps[id] = []esxi.Snapshot{{ID: fmt.Sprint(id), Name: fmt.Sprintf("cs4s-x-%d", id), Desc: ESXiTag}}
	}
	removed, errs := CleanupESXi(context.Background(), tr, vms)
	if len(errs) != 0 || len(removed) != 20 {
		t.Fatalf("removed %d errs %v", len(removed), errs)
	}
	for i, r := range removed {
		if !strings.HasPrefix(r, fmt.Sprintf("vm %d ", i+1)) {
			t.Fatalf("order broken at %d: %s", i, r)
		}
	}
}
