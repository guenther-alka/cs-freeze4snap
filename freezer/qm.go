package freezer

// Proxmox "memory" step: a hot VM snapshot with the RAM state, taken with
//
//	qm snapshot <vmid> cs4s_<zfs snapshot> --vmstate 1 --description "cs-freeze4snap <zfs snapshot>"
//
// It works without a guest agent: qm stops the VM for a moment, writes the RAM
// into a vmstate volume and snapshots the disks at the same point.
//
// Unlike ESXi the snapshot is NOT removed after the ZFS snapshot. On ZFS the
// state that qm snapshot preserves lives in ZFS snapshots of the disk zvols
// (vm-100-disk-0@cs4s_x) and in the vmstate zvol - removing it would destroy
// exactly the state that makes a restore consistent (the recursive ZFS snapshot
// taken afterwards holds the disks a moment LATER than the RAM). The snapshot
// stays for as long as the ZFS snapshot named in its description exists: every
// run removes the cs4s_ snapshots of a VM whose ZFS snapshot is gone (retention
// of the snapshot job) and, when "memkeep=N" is set, all but the newest N.
// Restore: qm rollback <vmid> cs4s_<name>.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// PVESnapPrefix starts the name of every VM snapshot this tool creates.
	PVESnapPrefix = "cs4s_"
	// PVETag starts the description of those snapshots; only snapshots with the
	// prefix AND the tag are ever removed by the tool.
	PVETag = "cs-freeze4snap"

	// StrategyQMMem is reported for a VM that got a qm memory snapshot.
	StrategyQMMem = "qm-mem"

	envQM         = "CS_FREEZE4SNAP_QM"      // path of qm (tests, non-standard installs)
	envPVEConfDir = "CS_FREEZE4SNAP_PVE_DIR" // directory of the <vmid>.conf files

	// defaultMemTimeout applies to a memory step without any timeout of its own
	// or of the chain: writing the RAM of a VM takes longer than a QGA freeze.
	defaultMemTimeout = 120 * time.Second
	// orphanGrace: a cs4s_ snapshot younger than this is never removed as an
	// orphan - a parallel job may not have taken its ZFS snapshot yet.
	orphanGrace = 10 * time.Minute
)

var rePVEBad = regexp.MustCompile(`[^A-Za-z0-9_]`)

var currentSnapName string

// SetSnapName tells the Proxmox freezer the name of the ZFS snapshot of this
// run; the VM snapshot is named after it and remembers it in its description.
func SetSnapName(s string) { currentSnapName = s }

// pveSnapName is cs4s_<zfs snapshot name> reduced to what Proxmox accepts for a
// snapshot name (letters, digits, _ ; at most 40 characters, the tail is kept
// because the run number sits there).
func pveSnapName(zfsName string) string {
	s := rePVEBad.ReplaceAllString(zfsName, "_")
	if s == "" {
		s = "snap"
	}
	if max := 40 - len(PVESnapPrefix); len(s) > max {
		s = s[len(s)-max:]
	}
	return PVESnapPrefix + s
}

// pveSnap is one snapshot section of a <vmid>.conf.
type pveSnap struct {
	Name  string
	Desc  string
	State string
	Time  int64
}

func (s pveSnap) tagged() bool {
	return strings.HasPrefix(s.Name, PVESnapPrefix) && strings.HasPrefix(s.Desc, PVETag)
}

// zfsName is the ZFS snapshot named in the description ("" if there is none).
func (s pveSnap) zfsName() string {
	return strings.TrimSpace(strings.TrimPrefix(s.Desc, PVETag))
}

// parsePVEConfSnaps reads the [snapshot] sections of a qemu-server config. The
// description is stored as "#..." comment lines (URL-escaped) right after the
// section header; a "description:" key is accepted as well.
func parsePVEConfSnaps(text string) []pveSnap {
	var out []pveSnap
	var cur *pveSnap
	var desc []string
	flush := func() {
		if cur != nil {
			if len(desc) > 0 && cur.Desc == "" {
				cur.Desc = strings.Join(desc, "\n")
			}
			out = append(out, *cur)
		}
		cur, desc = nil, nil
	}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			flush()
			cur = &pveSnap{Name: l[1 : len(l)-1]}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(l, "#") {
			if d, err := url.PathUnescape(l[1:]); err == nil {
				desc = append(desc, d)
			} else {
				desc = append(desc, l[1:])
			}
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "snaptime":
			cur.Time, _ = strconv.ParseInt(v, 10, 64)
		case "snapstate":
			cur.State = v
		case "description":
			if d, err := url.PathUnescape(v); err == nil {
				cur.Desc = d
			} else {
				cur.Desc = v
			}
		}
	}
	flush()
	return out
}

func pveConfDir() string {
	if d := os.Getenv(envPVEConfDir); d != "" {
		return d
	}
	return "/etc/pve/qemu-server"
}

func readPVESnaps(vmid int) ([]pveSnap, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d.conf", pveConfDir(), vmid))
	if err != nil {
		return nil, err
	}
	return parsePVEConfSnaps(string(b)), nil
}

func qmBin() string {
	if v := os.Getenv(envQM); v != "" {
		return v
	}
	return "qm"
}

// runQM runs qm with the context's deadline. The whole process group is killed
// on timeout; Proxmox may still finish the snapshot in a detached worker, which
// is why a failed step is reaped afterwards and leftovers are pruned later.
func runQM(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, qmBin(), args...)
	setProcGroup(cmd)
	cmd.Cancel = func() error { return killProcGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		last := out
		if i := strings.LastIndex(strings.TrimSpace(out), "\n"); i >= 0 {
			last = strings.TrimSpace(out[i+1:])
		}
		if len(last) > 300 {
			last = last[:300]
		}
		return out, fmt.Errorf("qm %s: %v: %s", strings.Join(args[:min(len(args), 2)], " "), err, last)
	}
	return out, nil
}

// deletePVESnap removes one snapshot; if the normal delete fails (a disk or the
// vmstate volume cannot be freed) it retries with --force so that the config
// does not keep a dead entry.
func deletePVESnap(vmid int, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := runQM(ctx, "delsnapshot", strconv.Itoa(vmid), name); err == nil {
		return nil
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if _, err := runQM(ctx2, "delsnapshot", strconv.Itoa(vmid), name, "--force", "1"); err != nil {
		return err
	}
	return nil
}

// zfsSnapExists reports whether dataset@snap exists; known is false when zfs
// could not answer (then nothing is removed).
var zfsSnapExists = func(dataset, snap string) (exists, known bool) {
	cmd := exec.Command("zfs", "list", "-H", "-o", "name", dataset+"@"+snap)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err == nil {
		return true, true
	}
	if strings.Contains(errb.String(), "does not exist") {
		return false, true
	}
	return false, false
}

// zfsSnapshotOfVM tells whether the ZFS snapshot zn still exists for the VM:
// on the disk zvol, or on the vmstate zvol of the memory snapshot vmSnap
// (vm-<id>-state-<vmSnap>, normally a sibling of the disk zvol). Both are looked
// at because a "zfs rollback -r" to the memory snapshot - the restore procedure -
// removes zn from the disk zvol only, while the RAM state of the snapshot in use
// still carries it. known is false when zfs could not answer.
func zfsSnapshotOfVM(g Guest, vmSnap, zn string) (exists, known bool) {
	ex, kn := zfsSnapExists(g.Dataset, zn)
	if kn && ex {
		return true, true
	}
	state := path.Join(path.Dir(g.Dataset), fmt.Sprintf("vm-%d-state-%s", g.VMID, vmSnap))
	if state != g.Dataset && path.Dir(g.Dataset) != "." {
		if ex2, kn2 := zfsSnapExists(state, zn); kn2 && ex2 {
			return true, true
		}
	}
	return ex, kn
}

// nowUnix is replaceable in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

// pruneMem removes the cs4s_ snapshots of one VM that are no longer needed:
// those whose ZFS snapshot is gone (older than orphanGrace), and - with
// final and memkeep > 0 - everything but the newest memkeep. keep is the
// snapshot taken in this run, it is never removed. It returns what it removed
// and what failed.
func pruneMem(g Guest, keep string, final bool) (removed, errs []string) {
	snaps, err := readPVESnaps(g.VMID)
	if err != nil {
		return nil, nil // no config: not a Proxmox host, or the VM is gone
	}
	var tagged []pveSnap
	for _, s := range snaps {
		if s.tagged() {
			tagged = append(tagged, s)
		}
	}
	if len(tagged) == 0 {
		return nil, nil
	}
	sort.SliceStable(tagged, func(i, j int) bool {
		if tagged[i].Time != tagged[j].Time {
			return tagged[i].Time > tagged[j].Time
		}
		return tagged[i].Name > tagged[j].Name
	})
	doomed := map[string]bool{}
	for _, s := range tagged {
		if s.Name == keep {
			continue
		}
		if nowUnix()-s.Time < int64(orphanGrace/time.Second) {
			continue
		}
		zn := s.zfsName()
		if zn == "" {
			doomed[s.Name] = true
			continue
		}
		if g.Dataset == "" {
			continue
		}
		if ex, known := zfsSnapshotOfVM(g, s.Name, zn); known && !ex {
			doomed[s.Name] = true
		}
	}
	if n := MemKeep(); final && n > 0 {
		kept := 0
		for _, s := range tagged {
			if doomed[s.Name] {
				continue
			}
			kept++
			if kept > n && s.Name != keep {
				doomed[s.Name] = true
			}
		}
	}
	for _, s := range tagged {
		if !doomed[s.Name] {
			continue
		}
		if err := deletePVESnap(g.VMID, s.Name); err != nil {
			errs = append(errs, fmt.Sprintf("old memory snapshot %s of vm %d could not be removed (qm delsnapshot %d %s --force 1): %v", s.Name, g.VMID, g.VMID, s.Name, err))
			continue
		}
		removed = append(removed, s.Name)
	}
	return removed, errs
}

// memSnapshot takes the qm snapshot with RAM state; the returned name is the
// VM snapshot. A step that fails or times out is reaped (the snapshot that qm
// may have left behind is removed again).
func memSnapshot(g Guest, qmp string, to time.Duration) (string, error) {
	if _, err := os.Stat(qmp); err != nil {
		return "", fmt.Errorf("vm is not running")
	}
	zn := currentSnapName
	name := pveSnapName(zn)
	// a leftover of the same name (crashed run)
	if snaps, err := readPVESnaps(g.VMID); err == nil {
		for _, s := range snaps {
			if s.Name == name && s.tagged() {
				_ = deletePVESnap(g.VMID, name)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	_, err := runQM(ctx, "snapshot", strconv.Itoa(g.VMID), name, "--vmstate", "1", "--description", PVETag+" "+zn)
	if err == nil {
		return name, nil
	}
	msg := err.Error()
	if ctx.Err() != nil {
		msg = fmt.Sprintf("timed out after %ds (qm snapshot cannot be cancelled cleanly)", int(to/time.Second))
	}
	if left := reapMem(g, name); left != "" {
		msg += " (" + left + ")"
	}
	return "", fmt.Errorf("%s", msg)
}

// reapMem removes a snapshot that a failed or timed-out qm snapshot may have
// left behind.
func reapMem(g Guest, name string) string {
	snaps, err := readPVESnaps(g.VMID)
	if err != nil {
		return ""
	}
	for _, s := range snaps {
		if s.Name == name {
			if err := deletePVESnap(g.VMID, name); err != nil {
				return fmt.Sprintf("leftover snapshot %s - remove it with: qm delsnapshot %d %s --force 1", name, g.VMID, name)
			}
		}
	}
	return ""
}
