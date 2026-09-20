//go:build linux

package freezer

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// envCgroupRoot overrides the default cgroup v2 mount point. Same rationale
// as CS_FREEZE4SNAP_QGA_DIR in qemu.go - testing and non-standard installs.
const envCgroupRoot = "CS_FREEZE4SNAP_CGROUP_ROOT"

// FIFREEZE/FITHAW are the standard Linux VFS ioctl numbers for freezing and
// thawing a filesystem (see linux/fs.h). golang.org/x/sys/unix does not
// currently export these (they're rarely used outside of backup/snapshot
// tooling like this one), so they're defined here directly. Values are
// stable across kernel versions - these are not expected to change.
const (
	ioctlFIFREEZE = 0xC0045877
	ioctlFITHAW   = 0xC0045878
)

// LXCFreezer freezes Proxmox LXC containers. Two strategies are supported:
//
//  1. Host-side fsfreeze (FIFREEZE ioctl) directly on the container's ZFS
//     mountpoint. This is the strongest consistency guarantee (forces a
//     journal flush, blocks new writes at the VFS layer) and - critically -
//     runs as a normal host operation on a normal host mount, so it works
//     the same for privileged and *unprivileged* containers. Calling
//     fsfreeze via `pct exec` instead would fail with EPERM on unprivileged
//     containers, because the container's rootfs mount is owned by the
//     host's user namespace, not the container's - see project notes.
//
//  2. cgroup v2 freeze (writing to cgroup.freeze) as a fallback, since not
//     all OpenZFS versions support FIFREEZE (confirmed on our own test
//     kernel: fsfreeze fails with "Operation not supported" - see
//     README/CHANGELOG). This stops all processes in the container but
//     does NOT force a flush by itself, so we issue an explicit sync of
//     the mountpoint first. Weaker guarantee than (1): dirty pages already
//     in cache but not yet written to ZFS are not specially handled beyond
//     that sync call.
//
// Which strategy is used is auto-detected per call and reported via the
// returned strategy string; it is not required to be identical across
// guests in the same run.
type LXCFreezer struct {
	// CgroupRoot allows overriding the cgroup v2 mount point, mainly for
	// testing. Defaults to /sys/fs/cgroup.
	CgroupRoot string
}

func init() {
	Register(&LXCFreezer{})
}

func (l *LXCFreezer) Name() string { return "lxc-fsfreeze" }

func (l *LXCFreezer) Supports(g Guest) bool {
	return g.Type == TypeLXC && g.Platform == PlatformProxmoxLXC
}

// mountpoint resolves the host filesystem mountpoint of the guest's ZFS
// dataset, e.g. /rpool/data/subvol-1234-disk-0.
func (l *LXCFreezer) mountpoint(g Guest) (string, error) {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", g.Dataset).Output()
	if err != nil {
		return "", fmt.Errorf("zfs get mountpoint %s: %w", g.Dataset, err)
	}
	mp := strings.TrimSpace(string(out))
	if mp == "" || mp == "none" || mp == "legacy" {
		return "", fmt.Errorf("dataset %s has no usable mountpoint (%q) - cannot fsfreeze", g.Dataset, mp)
	}
	return mp, nil
}

func (l *LXCFreezer) cgroupFreezePath(vmid int) string {
	root := l.CgroupRoot
	if root == "" {
		root = os.Getenv(envCgroupRoot)
	}
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	// Proxmox's default LXC cgroup layout. Verify this matches your PVE
	// version - the exact path has changed across PVE releases.
	return fmt.Sprintf("%s/lxc/%d/cgroup.freeze", root, vmid)
}

// ChainOf is the chain that applies to g: policy first, then freeze.
func (l *LXCFreezer) ChainOf(g Guest) Chain { return ChainFor(g, DefaultLXCChain) }

func (l *LXCFreezer) Freeze(g Guest, timeout time.Duration) (string, error) {
	ch := l.ChainOf(g)
	if steps, unsupported := ch.StepsFor(PlatformProxmoxLXC); len(steps) == 0 {
		if ch.ZFS && len(unsupported) == 0 {
			return StrategyZFSOnly, nil
		}
		return "", fmt.Errorf("chain %q has no step that works on a container (freeze)", ch)
	}
	mp, err := l.mountpoint(g)
	if err != nil {
		return "", err
	}

	// Strategy 1: host-side fsfreeze.
	fd, ferr := unix.Open(mp, unix.O_RDONLY, 0)
	if ferr == nil {
		if err := unix.IoctlSetInt(fd, ioctlFIFREEZE, 0); err == nil {
			freezeHandles.set(g.VMID, fd)
			return "fsfreeze", nil
		}
		unix.Close(fd)
		// fall through to cgroup strategy; common cause: ZFS on this
		// kernel/OpenZFS version does not implement FIFREEZE
		// (historically ENOTTY/ENOSYS/EOPNOTSUPP on ZFS).
	}

	// Strategy 2: sync + cgroup v2 freeze.
	syncCmd := exec.Command("sync", mp)
	if out, err := syncCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("sync %s before cgroup freeze: %w (%s)", mp, err, strings.TrimSpace(string(out)))
	}
	cgpath := l.cgroupFreezePath(g.VMID)
	if err := os.WriteFile(cgpath, []byte("1"), 0644); err != nil {
		return "", fmt.Errorf("both fsfreeze and cgroup freeze failed for vmid %d (fsfreeze: %v; cgroup write to %s: %w)", g.VMID, ferr, cgpath, err)
	}
	return "cgroup", nil
}

func (l *LXCFreezer) Thaw(g Guest) error {
	if fd, ok := freezeHandles.get(g.VMID); ok {
		defer freezeHandles.delete(g.VMID)
		defer unix.Close(fd)
		if err := unix.IoctlSetInt(fd, ioctlFITHAW, 0); err != nil {
			return fmt.Errorf("FITHAW failed for vmid %d: %w", g.VMID, err)
		}
		return nil
	}

	// Wasn't fsfrozen -> assume cgroup strategy was used, unfreeze that way.
	// Safe to call even if it was never frozen via cgroup either (writing
	// "0" to an already-thawed cgroup.freeze is a no-op).
	cgpath := l.cgroupFreezePath(g.VMID)
	if err := os.WriteFile(cgpath, []byte("0"), 0644); err != nil {
		return fmt.Errorf("cgroup thaw failed for vmid %d (%s): %w", g.VMID, cgpath, err)
	}
	return nil
}
