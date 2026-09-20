package freezer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cs-freeze4snap/esxi"
)

// ESXi modes: what the VM snapshot that stands in for a "freeze" contains.
const (
	ModeQuiesce = "quiesce" // VMware Tools flush the guest filesystem first (default)
	ModeMem     = "mem"     // the RAM state is kept too - a hot snapshot
	ModePlain   = "plain"   // disks only, crash-consistent
)

// Strategy identifiers for ESXi guests.
const (
	StrategyESXiQuiesce = "esxi-quiesce"
	StrategyESXiMem     = "esxi-mem"
	StrategyESXiSnap    = "esxi-snap" // plain snapshot: crash-consistent only

	// ESXiTag is written into the description of every VM snapshot this tool
	// creates; cleanup removes only snapshots carrying it.
	ESXiTag = "cs-freeze4snap"
)

var reBadName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// ESXiFreezer "freezes" a VM on a remote ESXi host by taking a VM snapshot
// (quiesced, with memory, or plain) and "thaws" it by removing that snapshot
// again. The ZFS snapshot taken in between then contains the VM's base disks
// frozen at the snapshot plus the delta, so the VM can be restored from the
// ZFS snapshot by reverting to the embedded ESXi snapshot.
//
// It works against any host reachable over ssh or soap, so the tool can run
// on any machine, not only on the ZFS server.
type ESXiFreezer struct {
	tr       esxi.Transport
	mode     string
	snapName string // the ZFS snapshot name, part of the VM snapshot name

	// ThawTimeout bounds one snapshot removal.
	ThawTimeout time.Duration

	mu    sync.Mutex
	snaps map[int]*esxiSnap
}

type esxiSnap struct {
	ID, Name, Strategy, Note string
}

// NewESXiFreezer creates the freezer; mode is quiesce, mem or plain.
func NewESXiFreezer(tr esxi.Transport, mode, snapName string) (*ESXiFreezer, error) {
	switch mode {
	case "":
		mode = ModeQuiesce
	case ModeQuiesce, ModeMem, ModePlain:
	default:
		return nil, fmt.Errorf("mode must be quiesce, mem or plain, not %q", mode)
	}
	return &ESXiFreezer{tr: tr, mode: mode, snapName: snapName, ThawTimeout: 2 * time.Minute, snaps: map[int]*esxiSnap{}}, nil
}

func (f *ESXiFreezer) Name() string          { return "esxi-" + f.tr.Name() }
func (f *ESXiFreezer) Supports(g Guest) bool { return g.Platform == PlatformESXi }

// vmSnapName is unique per run and VM and only holds characters that are safe
// in a shell command.
func (f *ESXiFreezer) vmSnapName(g Guest) string {
	n := "cs4s-" + reBadName.ReplaceAllString(f.snapName, "_")
	if len(n) > 48 {
		n = n[:48]
	}
	return fmt.Sprintf("%s-%d", n, g.VMID)
}

// Freeze takes the VM snapshot, trying progressively weaker variants:
// quiesce (or memory) first, then a plain disk snapshot.
func (f *ESXiFreezer) Freeze(g Guest, timeout time.Duration) (string, error) {
	type try struct {
		mem, quiesce bool
		strategy     string
	}
	var tries []try
	switch f.mode {
	case ModeMem:
		tries = []try{{true, false, StrategyESXiMem}, {false, false, StrategyESXiSnap}}
	case ModePlain:
		tries = []try{{false, false, StrategyESXiSnap}}
	default:
		tries = []try{{false, true, StrategyESXiQuiesce}, {false, false, StrategyESXiSnap}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name := f.vmSnapName(g)
	var errs []string
	for i, t := range tries {
		id, err := f.tr.CreateSnapshot(ctx, g.VMID, name, ESXiTag, t.mem, t.quiesce)
		if err == nil {
			s := &esxiSnap{ID: id, Name: name, Strategy: t.strategy}
			if i > 0 {
				s.Note = fmt.Sprintf("%s failed (%s) - took a plain snapshot instead: crash-consistent only", tries[0].strategy, strings.Join(errs, "; "))
			}
			f.mu.Lock()
			f.snaps[g.VMID] = s
			f.mu.Unlock()
			return t.strategy, nil
		}
		errs = append(errs, err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return "", fmt.Errorf("vm snapshot failed: %s", strings.Join(errs, "; "))
}

// Thaw removes the VM snapshot Freeze took. A guest without one is a no-op.
func (f *ESXiFreezer) Thaw(g Guest) error {
	f.mu.Lock()
	s := f.snaps[g.VMID]
	f.mu.Unlock()
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.ThawTimeout)
	defer cancel()
	if err := f.tr.RemoveSnapshot(ctx, g.VMID, s.ID); err != nil {
		// Already gone (removed by hand, by cleanup, or by an earlier thaw that
		// only lost the answer)? Then the VM is thawed and there is nothing to report.
		if snaps, lerr := f.tr.Snapshots(ctx, g.VMID); lerr == nil {
			found := false
			for _, sn := range snaps {
				if sn.ID == s.ID {
					found = true
				}
			}
			if !found {
				f.mu.Lock()
				delete(f.snaps, g.VMID)
				f.mu.Unlock()
				return nil
			}
		}
		return fmt.Errorf("remove vm snapshot %s (%s) - remove it by hand or run cleanup: %w", s.Name, s.ID, err)
	}
	f.mu.Lock()
	delete(f.snaps, g.VMID)
	f.mu.Unlock()
	return nil
}

// Note explains a degraded freeze (used for the Warning field).
func (f *ESXiFreezer) Note(g Guest) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.snaps[g.VMID]; s != nil {
		return s.Note
	}
	return ""
}

// SnapID reports the VM snapshot id taken for g ("" if none).
func (f *ESXiFreezer) SnapID(g Guest) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.snaps[g.VMID]; s != nil {
		return s.ID
	}
	return ""
}

// ESXiStateGuest is one frozen VM in a state file.
type ESXiStateGuest struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name,omitempty"`
	SnapID   string `json:"snap_id"`
	SnapName string `json:"snap_name"`
	Strategy string `json:"strategy"`
}

// ESXiState is written by "freeze" and read by "thaw": the VM snapshots that
// have to be removed once the ZFS snapshot exists.
type ESXiState struct {
	Version  int              `json:"version"`
	Created  string           `json:"created"`
	Host     string           `json:"host"`
	Dataset  string           `json:"dataset"`
	Snapshot string           `json:"snapshot"`
	Guests   []ESXiStateGuest `json:"guests"`
}

// State lists the VM snapshots currently held, ordered by VM id.
func (f *ESXiFreezer) State() []ESXiStateGuest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ESXiStateGuest
	for id, s := range f.snaps {
		out = append(out, ESXiStateGuest{VMID: id, SnapID: s.ID, SnapName: s.Name, Strategy: s.Strategy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

// Restore loads the snapshots of an earlier freeze so Thaw can remove them.
func (f *ESXiFreezer) Restore(gs []ESXiStateGuest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range gs {
		f.snaps[g.VMID] = &esxiSnap{ID: g.SnapID, Name: g.SnapName, Strategy: g.Strategy}
	}
}

// WriteESXiState writes the state file atomically with mode 0600.
func WriteESXiState(path string, st ESXiState) error {
	st.Version = 1
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadESXiState reads a state file written by WriteESXiState.
func ReadESXiState(path string) (ESXiState, error) {
	var st ESXiState
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("state file %s: %w", path, err)
	}
	if st.Version != 1 {
		return st, fmt.Errorf("state file %s: unsupported version %d", path, st.Version)
	}
	return st, nil
}

// CleanupESXi removes every leftover VM snapshot that carries ESXiTag on the
// given VMs (after a crashed run, or a Freeze that timed out while the host
// went on creating the snapshot). It returns what it removed and any errors.
func CleanupESXi(ctx context.Context, tr esxi.Transport, vms []esxi.VM) (removed []string, errs []string) {
	// One goroutine per VM, at most 8 at a time (ssh needs ~0.7s per vim-cmd call,
	// a host with dozens of VMs would take minutes one by one). Results are
	// collected per VM so the output stays in VM order.
	type result struct{ removed, errs []string }
	res := make([]result, len(vms))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, vm := range vms {
		wg.Add(1)
		go func(i int, vm esxi.VM) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &res[i]
			snaps, err := tr.Snapshots(ctx, vm.ID)
			if err != nil {
				r.errs = append(r.errs, fmt.Sprintf("vm %d (%s): %v", vm.ID, vm.Name, err))
				return
			}
			for _, s := range snaps {
				if s.Desc != ESXiTag || !strings.HasPrefix(s.Name, "cs4s-") {
					continue
				}
				if err := tr.RemoveSnapshot(ctx, vm.ID, s.ID); err != nil {
					r.errs = append(r.errs, fmt.Sprintf("vm %d (%s) snapshot %s: %v", vm.ID, vm.Name, s.Name, err))
					continue
				}
				r.removed = append(r.removed, fmt.Sprintf("vm %d (%s): %s", vm.ID, vm.Name, s.Name))
			}
		}(i, vm)
	}
	wg.Wait()
	for _, r := range res {
		removed = append(removed, r.removed...)
		errs = append(errs, r.errs...)
	}
	return removed, errs
}
