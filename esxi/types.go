// Package esxi talks to a standalone ESXi host and answers the two questions
// cs-freeze4snap needs: "which VMs live on this NFS export?" and "snapshot /
// un-snapshot this VM". Two transports are provided:
//
//   - ssh:  vim-cmd / esxcli over SSH (works on the free ESXi license,
//     nothing to install on the host except enabling SSH)
//   - soap: the vSphere Web Services API on https://host/sdk (password login,
//     no SSH needed). The free license only accepts write calls from clients
//     that identify themselves as a VMware client (User-Agent "VMware ..."),
//     so the transport sends one - see userAgentDefault in soap.go.
//
// The package has no dependency on the rest of cs-freeze4snap.
package esxi

import "context"

// Power states, normalised to the names the SOAP API uses.
const (
	PowerOn        = "poweredOn"
	PowerOff       = "poweredOff"
	PowerSuspended = "suspended"
)

// Datastore is one datastore of the ESXi host. For NFS datastores Host and
// Path are the NFS server and the exported path (e.g. 192.168.2.203, /tank/nfs).
type Datastore struct {
	Name string `json:"name"`
	Type string `json:"type"` // NFS, NFS41, VMFS, ...
	Host string `json:"host,omitempty"`
	Path string `json:"path,omitempty"`
}

// IsNFS reports whether the datastore is an NFS mount.
func (d Datastore) IsNFS() bool { return d.Type == "NFS" || d.Type == "NFS41" }

// VM is a registered virtual machine. Datastores lists the names of every
// datastore the VM has files or disks on.
type VM struct {
	ID         int      `json:"vmid"`
	Name       string   `json:"name"`
	PowerState string   `json:"power"`
	Datastores []string `json:"datastores,omitempty"`
}

// Snapshot is one VM snapshot. ID is transport specific (vim-cmd: a number,
// SOAP: a managed object reference such as "25-snapshot-5") and only has to
// be handed back to RemoveSnapshot unchanged.
type Snapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
}

// Transport is everything cs-freeze4snap needs from an ESXi host.
type Transport interface {
	Name() string // "ssh" or "soap"
	Datastores(ctx context.Context) ([]Datastore, error)
	VMs(ctx context.Context) ([]VM, error)
	Snapshots(ctx context.Context, vmid int) ([]Snapshot, error)
	// CreateSnapshot returns the ID of the new snapshot. mem keeps the RAM
	// state, quiesce asks VMware Tools to flush the guest filesystem.
	CreateSnapshot(ctx context.Context, vmid int, name, desc string, mem, quiesce bool) (string, error)
	RemoveSnapshot(ctx context.Context, vmid int, snapID string) error
	// Warnings collects non-fatal findings (e.g. unpinned host key).
	Warnings() []string
	Close() error
}
