package esxi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// DiscoverOpts selects the VMs that live on one NFS export.
type DiscoverOpts struct {
	Path       string // NFS export path, e.g. /tank/nfs (the dataset's mountpoint)
	Recursive  bool   // also datastores exported from below Path (child datasets)
	Server     string // NFS server ip/name as the ESXi host knows it; "" = any (must then be unambiguous)
	VMs        string // "all" (default) or a comma list of VM ids and/or names
	AllowMixed bool   // snapshot VMs that also have files on other datastores
	IncludeOff bool   // also snapshot powered-off / suspended VMs (normally pointless)
}

// Skipped is a VM that lives on the NFS export but is not snapshotted, with the reason.
type Skipped struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Discovery is the answer of DiscoverNFS.
type Discovery struct {
	Datastores []Datastore `json:"datastores"` // NFS datastores matching the export
	VMs        []VM        `json:"vms"`        // VMs to snapshot (powered on)
	OnNFS      []VM        `json:"-"`          // every VM with files on these datastores, before any filter (for cleanup)
	Skipped    []Skipped   `json:"skipped,omitempty"`
	Warnings   []string    `json:"warnings,omitempty"`
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = "/" + strings.Trim(p, "/")
	return p
}

// DiscoverNFS finds the NFS datastores of the host that mount the given
// export and the VMs that live on them. Only powered-on VMs need a snapshot
// to get a consistent state - powered-off and suspended VMs are reported as
// skipped. A VM that also has disks on another datastore is skipped too
// (a ZFS snapshot of the NFS export would capture only part of it) unless
// AllowMixed is set.
func DiscoverNFS(ctx context.Context, tr Transport, o DiscoverOpts) (*Discovery, error) {
	root := normPath(o.Path)
	if root == "" {
		return nil, fmt.Errorf("no NFS export path")
	}
	all, err := tr.Datastores(ctx)
	if err != nil {
		return nil, fmt.Errorf("list datastores: %w", err)
	}
	d := &Discovery{}
	hosts := map[string]bool{}
	for _, ds := range all {
		if !ds.IsNFS() {
			continue
		}
		p := normPath(ds.Path)
		if !(p == root || (o.Recursive && strings.HasPrefix(p, root+"/"))) {
			continue
		}
		if o.Server != "" && !strings.EqualFold(ds.Host, o.Server) {
			continue
		}
		d.Datastores = append(d.Datastores, ds)
		hosts[strings.ToLower(ds.Host)] = true
	}
	if len(d.Datastores) == 0 {
		return nil, fmt.Errorf("no NFS datastore on this ESXi host mounts %s%s%s - is the export mounted, and does --nfs-path / --nfs-server match what 'esxcli storage nfs list' shows?",
			root, map[bool]string{true: " (or below)", false: ""}[o.Recursive], map[bool]string{true: " from " + o.Server, false: ""}[o.Server != ""])
	}
	if len(hosts) > 1 {
		var hs []string
		for h := range hosts {
			hs = append(hs, h)
		}
		sort.Strings(hs)
		return nil, fmt.Errorf("%s is exported by more than one NFS server (%s) - set --nfs-server", root, strings.Join(hs, ", "))
	}
	sel := map[string]bool{}
	for _, ds := range d.Datastores {
		sel[ds.Name] = true
	}

	want, err := parseVMFilter(o.VMs)
	if err != nil {
		return nil, err
	}
	vms, err := tr.VMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	for _, vm := range vms {
		var on, off []string
		for _, n := range vm.Datastores {
			if sel[n] {
				on = append(on, n)
			} else {
				off = append(off, n)
			}
		}
		if len(on) == 0 {
			continue // not our NFS
		}
		d.OnNFS = append(d.OnNFS, vm)
		if want != nil && !want.matches(vm) {
			d.Skipped = append(d.Skipped, Skipped{vm.ID, vm.Name, "not in the --vms list"})
			continue
		}
		switch {
		case vm.PowerState == PowerOff && !o.IncludeOff:
			d.Skipped = append(d.Skipped, Skipped{vm.ID, vm.Name, "powered off - consistent without a VM snapshot"})
		case vm.PowerState == PowerSuspended && !o.IncludeOff:
			d.Skipped = append(d.Skipped, Skipped{vm.ID, vm.Name, "suspended - consistent without a VM snapshot"})
		case len(off) > 0 && !o.AllowMixed:
			d.Skipped = append(d.Skipped, Skipped{vm.ID, vm.Name, "has files on other datastore(s) " + strings.Join(off, ", ") + " - a ZFS snapshot of this NFS would capture only part of the VM (--allow-mixed to snapshot anyway)"})
		default:
			if len(off) > 0 {
				d.Warnings = append(d.Warnings, fmt.Sprintf("vm %d (%s) also has files on %s - not part of the ZFS snapshot", vm.ID, vm.Name, strings.Join(off, ", ")))
			}
			d.VMs = append(d.VMs, vm)
		}
	}
	return d, nil
}

type vmFilter struct {
	ids   map[int]bool
	names map[string]bool
}

func (f *vmFilter) matches(vm VM) bool { return f.ids[vm.ID] || f.names[vm.Name] }

// parseVMFilter: "" or "all" means every VM (nil filter).
func parseVMFilter(s string) (*vmFilter, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "all") {
		return nil, nil
	}
	f := &vmFilter{ids: map[int]bool{}, names: map[string]bool{}}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			f.ids[n] = true
		} else {
			f.names[p] = true
		}
	}
	return f, nil
}
