package freezer

import (
	"sync"
	"time"
)

// GuestResult reports the freeze/thaw outcome for one guest, for logging
// and JSON output. There is no "failed the whole job" state here by
// design: a guest that could not be frozen at all is simply recorded with
// Frozen=false and a Warning explaining why - the ZFS snapshot proceeds
// regardless (as-is / crash-consistent for that guest). Nothing in this
// package aborts the overall operation due to a freeze problem, whether
// that's "no guest agent configured", "Proxmox/QMP didn't respond", or any
// other freeze-side failure.
type GuestResult struct {
	VMID     int       `json:"vmid"`
	Name     string    `json:"name,omitempty"` // VM name (ESXi)
	Type     GuestType `json:"type"`
	Platform Platform  `json:"platform"`
	Frozen   bool      `json:"frozen"`
	Strategy string    `json:"strategy,omitempty"` // e.g. "qga", "qmp-pause", "fsfreeze", "cgroup", "none"
	Thawed   bool      `json:"thawed"`
	// Warning explains why this guest was not frozen (or only partially),
	// covering everything from "no freeze method available" to "no
	// freezer registered for this platform" to an unexpected internal
	// error from the Freezer implementation. Purely informational - never
	// causes the snapshot to be skipped or the job to abort.
	Warning string `json:"warning,omitempty"`
	// SnapID is the VM snapshot that stands in for the freeze (ESXi): the
	// handle "thaw" needs to remove it again.
	SnapID string `json:"snap_id,omitempty"`
	// Chain is the freeze chain that applied to this guest ("freeze,memory,zfs,30"),
	// Strict that it had no zfs fallback: a guest that is not Frozen then aborts the run.
	Chain  string `json:"chain,omitempty"`
	Strict bool   `json:"strict,omitempty"`
}

// chainer is implemented by Freezers that resolve a chain per guest.
type chainer interface {
	ChainOf(g Guest) Chain
}

// noter is optionally implemented by a Freezer that can explain a degraded
// but successful freeze (e.g. "freeze failed, plain snapshot taken").
type noter interface {
	Note(g Guest) string
}

// FreezeSession represents one in-progress freeze operation across a set of
// guests. Created by FreezeAll, always followed by a call to Thaw (typically
// via defer) regardless of what happens in between.
type FreezeSession struct {
	results []GuestResult
	frozen  []Guest // guests with a real Frozen strategy - only these get thawed
}

// FreezeAll attempts to freeze every guest in parallel so they all pause at
// approximately the same instant (important when snapshotting a dataset
// shared by multiple guests). This is always best-effort: whatever happens
// to any individual guest - no freezer registered for its platform, no
// guest agent, Proxmox/QMP not responding, any other error - is recorded
// as a Warning on that guest's result and never prevents the caller from
// proceeding to snapshot. There is no abort path; the goal is "freeze if
// at all possible, snapshot regardless."
func FreezeAll(guests []Guest, timeout time.Duration) *FreezeSession {
	sess := &FreezeSession{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, g := range guests {
		wg.Add(1)
		go func(g Guest) {
			defer wg.Done()
			res := GuestResult{VMID: g.VMID, Name: g.Name, Type: g.Type, Platform: g.Platform}

			f, err := For(g)
			ch := ChainFor(g, DefaultChainFor(g.Platform, g.Type))
			if c, ok := f.(chainer); ok && err == nil {
				ch = c.ChainOf(g)
			}
			res.Chain, res.Strict = ch.String(), ch.Strict()
			if err != nil {
				res.Warning = err.Error()
				mu.Lock()
				sess.results = append(sess.results, res)
				mu.Unlock()
				return
			}

			strategy, err := f.Freeze(g, timeout)
			res.Strategy = strategy
			switch {
			case err != nil:
				res.Warning = err.Error()
			case strategy == StrategyZFSOnly:
				res.Warning = "no guest freeze (chain: zfs) - ZFS snapshot only"
			case strategy == StrategyNone:
				res.Warning = "no freeze method available for this guest - snapshotting as-is (crash-consistent only)"
			default:
				res.Frozen = true
			}
			if n, ok := f.(noter); ok && res.Frozen {
				res.Warning = n.Note(g)
			}
			if h, ok := f.(interface{ SnapID(g Guest) string }); ok {
				res.SnapID = h.SnapID(g)
			}

			mu.Lock()
			sess.results = append(sess.results, res)
			if res.Frozen {
				sess.frozen = append(sess.frozen, g)
			}
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	return sess
}

// Thaw resumes every guest that was successfully frozen in this session.
// Safe to call multiple times and safe to call even if nothing froze - it
// only ever touches guests it actually froze. Intended to be called via
// defer immediately after FreezeAll returns.
func (s *FreezeSession) Thaw() {
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, g := range s.frozen {
		wg.Add(1)
		go func(g Guest) {
			defer wg.Done()
			f, err := For(g) // already known to support g, from Freeze phase
			mu.Lock()
			defer mu.Unlock()
			idx := s.findResult(g.VMID)
			if err != nil {
				if idx >= 0 {
					s.results[idx].Warning = "thaw: " + err.Error()
				}
				return
			}
			if err := f.Thaw(g); err != nil {
				if idx >= 0 {
					s.results[idx].Warning = "thaw: " + err.Error()
				}
				return
			}
			if idx >= 0 {
				s.results[idx].Thawed = true
			}
		}(g)
	}
	wg.Wait()
	s.frozen = nil // idempotent: a second Thaw() call becomes a no-op
}

func (s *FreezeSession) findResult(vmid int) int {
	for i, r := range s.results {
		if r.VMID == vmid {
			return i
		}
	}
	return -1
}

// Results returns the current per-guest outcome list, safe to call after
// FreezeAll and/or Thaw.
func (s *FreezeSession) Results() []GuestResult {
	return s.results
}

// RestoreSession rebuilds a session for guests that an earlier process froze
// (cs-freeze4snap freeze ... / thaw ...), so that Thaw() can undo it.
func RestoreSession(guests []Guest) *FreezeSession {
	s := &FreezeSession{}
	for _, g := range guests {
		s.results = append(s.results, GuestResult{VMID: g.VMID, Name: g.Name, Type: g.Type, Platform: g.Platform, Frozen: true})
		s.frozen = append(s.frozen, g)
	}
	return s
}
