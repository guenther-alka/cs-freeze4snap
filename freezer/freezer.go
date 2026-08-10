// Package freezer defines the pluggable guest-freeze abstraction used by
// cs-freeze4snap. Each hypervisor/guest-type combination implements the
// Freezer interface and registers itself via Register(). The orchestrator
// (main.go) never knows about QEMU, LXC, Hyper-V, or bhyve directly - it
// only talks to whichever Freezer claims to Support() a given guest.
//
// To add a new platform later (Hyper-V, bhyve, ...):
//  1. Implement the Freezer interface in a new file (e.g. freezer_hyperv.go)
//  2. Call freezer.Register(&HyperVFreezer{}) from an init() in that file
//  3. Nothing else changes - main.go and discovery stay untouched.
package freezer

import (
	"fmt"
	"time"
)

// GuestType identifies what kind of guest we're dealing with. Kept as a
// string (not iota) so new platforms can introduce new values without
// renumbering anything.
type GuestType string

const (
	TypeVM  GuestType = "vm"  // QEMU/KVM, Hyper-V VM, bhyve VM, ...
	TypeLXC GuestType = "lxc" // Linux containers (Proxmox LXC, ...)
)

// Platform identifies the hypervisor/host stack a guest runs under. This is
// what lets multiple Freezer implementations coexist for the same GuestType
// (e.g. TypeVM under "qemu" vs TypeVM under "hyperv").
type Platform string

const (
	PlatformProxmoxQEMU Platform = "proxmox-qemu"
	PlatformProxmoxLXC  Platform = "proxmox-lxc"
	PlatformHyperV      Platform = "hyperv" // not yet implemented
	PlatformBhyve       Platform = "bhyve"  // not yet implemented
)

// Guest describes one discovered guest that may need to be frozen.
type Guest struct {
	VMID     int
	Type     GuestType
	Platform Platform
	Dataset  string // ZFS dataset/zvol backing this guest (for logging)
}

func (g Guest) String() string {
	return fmt.Sprintf("%s/%d(%s)", g.Platform, g.VMID, g.Type)
}

// Freezer pauses and resumes guest I/O so a storage-level snapshot taken
// while frozen is application-consistent (or at minimum filesystem-
// consistent). Implementations must make Thaw safe to call even if Freeze
// never succeeded or only partially completed - the orchestrator always
// calls Thaw via defer.
type Freezer interface {
	// Name is a short human-readable identifier for logging, e.g. "qemu-qga".
	Name() string

	// Supports reports whether this Freezer can handle the given guest.
	Supports(g Guest) bool

	// Freeze attempts to pause guest I/O, trying progressively weaker
	// strategies as needed, and reports which one actually worked via the
	// returned strategy string (implementation-defined, e.g. "qga",
	// "qmp-pause", "fsfreeze", "cgroup"). Never treated as fatal by the
	// caller regardless of outcome - the snapshot proceeds either way.
	//
	// A returned error means Freeze couldn't determine anything useful for
	// this guest (e.g. no Freezer registered, or an unexpected internal
	// failure). Implementations that offer a deliberate "best effort,
	// nothing better available" degraded mode (e.g. QEMUFreezer falling
	// back to snapshotting a VM as-is when neither QGA nor QMP pause are
	// possible) should prefer returning strategy "none" with a NIL error
	// instead of an error, since that's an expected, common outcome (e.g.
	// simply "no guest agent configured") rather than something unusual -
	// but either way, the caller never aborts on this.
	Freeze(g Guest, timeout time.Duration) (strategy string, err error)

	// Thaw resumes guest I/O, undoing whatever the last successful Freeze
	// call did (it must remember which strategy was used per guest).
	// Called unconditionally after Freeze was attempted, even if Freeze
	// returned an error or the operation in between (e.g. zfs snapshot)
	// failed. Must not panic and should log/return an error rather than
	// leaving the guest stuck if at all avoidable. Thaw for a guest that
	// was frozen with strategy "none" must be a safe no-op.
	Thaw(g Guest) error
}

var registry []Freezer

// Register adds a Freezer implementation to the global registry. Called
// from init() in each platform-specific file.
func Register(f Freezer) {
	registry = append(registry, f)
}

// For returns the first registered Freezer that supports the given guest.
func For(g Guest) (Freezer, error) {
	for _, f := range registry {
		if f.Supports(g) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no freezer registered for guest %s - platform %q not supported yet", g, g.Platform)
}
