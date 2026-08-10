package freezer

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const platformTest Platform = "test"

// fakeFreezer is a controllable, in-memory Freezer used only by tests. It
// lets us simulate slow guests, failing guests, and "no method available"
// guests, and verify Thaw is called exactly for the guests that actually
// froze.
type fakeFreezer struct {
	mu          sync.Mutex
	failVMIDs   map[int]bool // Freeze returns a genuine error
	noneVMIDs   map[int]bool // Freeze returns strategy "none", nil error
	slowVMIDs   map[int]time.Duration
	frozenCalls []int
	thawedCalls []int
}

func newFakeFreezer() *fakeFreezer {
	return &fakeFreezer{
		failVMIDs: map[int]bool{},
		noneVMIDs: map[int]bool{},
		slowVMIDs: map[int]time.Duration{},
	}
}

func (f *fakeFreezer) Name() string          { return "fake" }
func (f *fakeFreezer) Supports(g Guest) bool { return g.Platform == platformTest }

func (f *fakeFreezer) Freeze(g Guest, timeout time.Duration) (string, error) {
	if d, ok := f.slowVMIDs[g.VMID]; ok {
		time.Sleep(d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failVMIDs[g.VMID] {
		return "", fmt.Errorf("simulated freeze failure for vmid %d", g.VMID)
	}
	if f.noneVMIDs[g.VMID] {
		return StrategyNone, nil
	}
	f.frozenCalls = append(f.frozenCalls, g.VMID)
	return "fake-strategy", nil
}

func (f *fakeFreezer) Thaw(g Guest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thawedCalls = append(f.thawedCalls, g.VMID)
	return nil
}

func TestFreezeAll_AllSucceed(t *testing.T) {
	ff := newFakeFreezer()
	Register(ff)
	defer unregisterLast()

	guests := []Guest{
		{VMID: 1, Platform: platformTest},
		{VMID: 2, Platform: platformTest},
		{VMID: 3, Platform: platformTest},
	}

	sess := FreezeAll(guests, time.Second)
	if len(ff.frozenCalls) != 3 {
		t.Errorf("expected 3 freeze calls, got %d: %v", len(ff.frozenCalls), ff.frozenCalls)
	}

	sess.Thaw()
	if len(ff.thawedCalls) != 3 {
		t.Errorf("expected 3 thaw calls, got %d: %v", len(ff.thawedCalls), ff.thawedCalls)
	}

	for _, r := range sess.Results() {
		if !r.Frozen || !r.Thawed || r.Warning != "" {
			t.Errorf("guest %d: expected clean success, got %+v", r.VMID, r)
		}
	}
}

func TestFreezeAll_NeverAborts_OnGenuineError(t *testing.T) {
	ff := newFakeFreezer()
	ff.failVMIDs[2] = true
	Register(ff)
	defer unregisterLast()

	guests := []Guest{
		{VMID: 1, Platform: platformTest},
		{VMID: 2, Platform: platformTest}, // errors
		{VMID: 3, Platform: platformTest},
	}

	// FreezeAll has no error return at all - there is no abort path.
	sess := FreezeAll(guests, time.Second)
	sess.Thaw()

	if len(ff.thawedCalls) != 2 { // only guests 1 and 3 actually froze
		t.Errorf("expected 2 thaw calls (only guests that froze), got %d: %v", len(ff.thawedCalls), ff.thawedCalls)
	}
	for _, vmid := range ff.thawedCalls {
		if vmid == 2 {
			t.Errorf("guest 2 never froze and must not be thawed, but was")
		}
	}

	var g2 *GuestResult
	for i := range sess.Results() {
		if sess.Results()[i].VMID == 2 {
			g2 = &sess.Results()[i]
		}
	}
	if g2 == nil {
		t.Fatal("expected a result for vmid 2")
	}
	if g2.Frozen {
		t.Error("vmid 2 should not be marked Frozen")
	}
	if g2.Warning == "" {
		t.Error("vmid 2 should have a Warning explaining the freeze failure")
	}
}

func TestFreezeAll_StrategyNone_IsWarningNotFatal(t *testing.T) {
	ff := newFakeFreezer()
	ff.noneVMIDs[101] = true // simulates "no QGA, no QMP" case
	Register(ff)
	defer unregisterLast()

	guests := []Guest{{VMID: 101, Platform: platformTest}}
	sess := FreezeAll(guests, time.Second)
	sess.Thaw()

	results := sess.Results()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Frozen {
		t.Error("strategy 'none' must not be marked Frozen")
	}
	if r.Strategy != StrategyNone {
		t.Errorf("expected strategy %q, got %q", StrategyNone, r.Strategy)
	}
	if r.Warning == "" {
		t.Error("expected a Warning for strategy 'none'")
	}
	// Since it never froze, Thaw must not have been called for it.
	if len(ff.thawedCalls) != 0 {
		t.Errorf("expected no thaw calls for a guest that was never frozen, got %v", ff.thawedCalls)
	}
}

func TestFreezeAll_RunsInParallel(t *testing.T) {
	ff := newFakeFreezer()
	const n = 5
	const perGuestDelay = 100 * time.Millisecond
	guests := make([]Guest, n)
	for i := 0; i < n; i++ {
		guests[i] = Guest{VMID: i, Platform: platformTest}
		ff.slowVMIDs[i] = perGuestDelay
	}
	Register(ff)
	defer unregisterLast()

	start := time.Now()
	sess := FreezeAll(guests, time.Second)
	elapsed := time.Since(start)
	sess.Thaw()

	// If freezing were sequential this would take n*perGuestDelay (500ms).
	// In parallel it should take roughly perGuestDelay regardless of n.
	if elapsed > perGuestDelay*3 {
		t.Errorf("FreezeAll took %v for %d guests at %v each - looks sequential, not parallel", elapsed, n, perGuestDelay)
	}
}

func TestFreezeAll_UnsupportedGuestPlatform_NeverAborts(t *testing.T) {
	// No freezer registered for this platform at all - must degrade to a
	// Warning, not stop anything.
	guests := []Guest{{VMID: 1, Platform: "nonexistent-platform"}}
	sess := FreezeAll(guests, time.Second)
	sess.Thaw() // must not panic even though nothing froze

	r := sess.Results()[0]
	if r.Frozen {
		t.Error("guest with no matching freezer must not be marked Frozen")
	}
	if r.Warning == "" {
		t.Error("expected a Warning explaining no freezer was found")
	}
}

// unregisterLast removes the most recently registered Freezer. Test-only
// helper to keep the global registry clean between tests, since Register()
// has no corresponding Unregister() in the production API (there's no
// legitimate runtime need to unregister a platform).
func unregisterLast() {
	if len(registry) > 0 {
		registry = registry[:len(registry)-1]
	}
}
