package main

// ESXi subcommands: snap --hypervisor esxi, discover, freeze, thaw, cleanup.
//
// The tool talks to the ESXi host over ssh (vim-cmd) or soap (vSphere API),
// so it may run on any machine that can reach the host - the ZFS server, a
// frontend, a jump host. The ZFS snapshot itself is taken locally
// (zfs snapshot) or, for a remote frontend, by --zfs-cmd.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"cs-freeze4snap/esxi"
	"cs-freeze4snap/freezer"
)

const esxiUsage = `  --hypervisor esxi        (snap only) hotsnap ESXi VMs instead of Proxmox guests
  --cfg file               ESXi connection file: a host table (host,user,password / host,cert) or
                             key=value (host, user, password, proto, port, key, key_passphrase,
                             hostkey, tls_sha256, useragent, timeout); it may also hold the freeze
                             chains, see freeze4snap.readme.
                             CS_ESXI_HOST / CS_ESXI_USER / CS_ESXI_PASSWORD override the file,
                             --host / --user / --proto override both. The password is never a flag.
  --proto auto|ssh|soap    transport (default auto: soap = vSphere API on port 443, ssh = vim-cmd
                             if soap cannot be reached; a failed soap login is not retried)
  --host, --user           ESXi host / login (default user root)
  --storage nfs            select the VMs by storage: all VMs on the NFS export of the dataset
  --nfs-path path          NFS export path as ESXi mounts it (default: mountpoint of --dataset)
  --nfs-server host        NFS server as ESXi knows it, needed if the path is exported twice
  --vms all|id,name,...    which VMs on that NFS (default all; --snap is an alias)
  --mode freeze|mem|plain  shortcut for a chain that applies to every VM: freeze = freeze,plain,zfs;
                             mem = memory,plain,zfs; plain = plain,zfs (default: the chains of the cfg
                             file, else freeze,memory,zfs; quiesce is an alias of freeze)
  --policy 'chain'         freeze chain, repeatable: '[freeze,memory,zfs,30]' for every guest,
                             'vm100,memory,zfs' for one VM ('*' for all). Steps: freeze, memory,
                             plain, pause (Proxmox VM), zfs; a number is a timeout in seconds,
                             'step:60' one step's. Without zfs at the end the chain is strict: a guest
                             that cannot be frozen aborts the run before the ZFS snapshot.
  --allow-mixed            also snapshot VMs that have disks on other datastores
  --include-off            also snapshot powered-off/suspended VMs (default: they are skipped,
                             they are consistent anyway)
  --thaw-timeout duration  max time to remove one VM snapshot (default 2m)
  --zfs-cmd 'command'      take the ZFS snapshot with this shell command instead of the local
                             zfs; {dataset} {snapshot} {fullname} are replaced, e.g.
                             --zfs-cmd 'ssh nas zfs snapshot -r {fullname}'
  --state file             freeze/thaw: file that carries the VM snapshots from freeze to thaw

Subcommands (ESXi):
  discover   list the NFS datastores and VMs that would be snapshotted (JSON), changes nothing
  freeze     take the VM snapshots, write --state, leave them in place
  thaw       remove the VM snapshots listed in --state
  cleanup    remove leftover VM snapshots created by this tool (tag cs-freeze4snap) on the NFS

With --forcefreeze a failing connection or VM snapshot aborts before the ZFS snapshot;
otherwise the ZFS snapshot is taken anyway and the problem is reported in "warnings".`

// esxiOpts are the flags shared by all ESXi commands.
type esxiOpts struct {
	hypervisor  string
	cfg         string
	proto       string
	host        string
	user        string
	storage     string
	nfsPath     string
	nfsServer   string
	vms         string
	mode        string
	policy      multiFlag
	allowMixed  bool
	includeOff  bool
	thawTimeout time.Duration
	zfsCmd      string
	state       string
}

func (o *esxiOpts) register(fs *flag.FlagSet, withHypervisor bool) {
	if withHypervisor {
		fs.StringVar(&o.hypervisor, "hypervisor", "proxmox", "proxmox or esxi")
	}
	fs.StringVar(&o.cfg, "cfg", "", "ESXi connection file (key=value)")
	fs.StringVar(&o.proto, "proto", "", "esxi transport: auto (default), ssh or soap")
	fs.StringVar(&o.host, "host", "", "esxi host")
	fs.StringVar(&o.user, "user", "", "esxi user")
	fs.StringVar(&o.storage, "storage", "nfs", "select VMs by storage type (only nfs)")
	fs.StringVar(&o.nfsPath, "nfs-path", "", "NFS export path as ESXi mounts it (default: mountpoint of --dataset)")
	fs.StringVar(&o.nfsServer, "nfs-server", "", "NFS server as ESXi knows it")
	fs.StringVar(&o.vms, "vms", "all", "all or comma list of VM ids/names")
	fs.StringVar(&o.vms, "snap", "all", "alias of --vms")
	fs.StringVar(&o.mode, "mode", "", "freeze, mem or plain (a chain for every VM)")
	fs.Var(&o.policy, "policy", "freeze chain, repeatable: '[freeze,memory,zfs,30]' or 'vm100,memory,zfs'")
	fs.BoolVar(&o.allowMixed, "allow-mixed", false, "also snapshot VMs with disks on other datastores")
	fs.BoolVar(&o.includeOff, "include-off", false, "also snapshot powered-off/suspended VMs")
	fs.DurationVar(&o.thawTimeout, "thaw-timeout", 2*time.Minute, "max time to remove one VM snapshot")
	fs.StringVar(&o.zfsCmd, "zfs-cmd", "", "shell command that takes the ZFS snapshot ({dataset} {snapshot} {fullname})")
	fs.StringVar(&o.state, "state", "", "state file for freeze/thaw")
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, " | ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// loadPolicy installs the freeze chains: the cfg file (lines of this server),
// then the --policy flags, then --mode; the strongest wins.
func (o *esxiOpts) loadPolicy(host string) error {
	p, err := freezer.LoadPolicy(o.cfg, host)
	if err != nil {
		return err
	}
	if len(o.policy) > 0 {
		fp, err := freezer.PolicyOfFlags(o.policy)
		if err != nil {
			return err
		}
		p = p.Merge(fp)
	}
	if o.mode != "" {
		c, ok := freezer.ChainOfMode(o.mode)
		if !ok {
			return fmt.Errorf("--mode must be freeze, mem or plain, not %q", o.mode)
		}
		p.SetOverride(c)
	}
	freezer.SetPolicy(p)
	return nil
}

// connect opens the transport: cfg file, then environment, then flags. With
// policy the freeze chains are loaded for the server that was resolved.
func (o *esxiOpts) connect(policy bool) (esxi.Transport, error) {
	if o.storage != "nfs" {
		return nil, fmt.Errorf("--storage must be nfs, not %q", o.storage)
	}
	// the server to use: --host, else CS_ESXI_HOST, else the only one in the cfg file
	host := o.host
	if host == "" {
		host = os.Getenv("CS_ESXI_HOST")
	}
	c, err := esxi.LoadConfigFor(o.cfg, host)
	if err != nil {
		return nil, err
	}
	c.ApplyEnv()
	if o.host != "" {
		c.Host = o.host
	}
	if o.user != "" {
		c.User = o.user
	}
	if o.proto != "" {
		c.Proto = strings.ToLower(o.proto)
	}
	if policy {
		if err := o.loadPolicy(c.Host); err != nil {
			return nil, err
		}
	}
	return esxi.Open(c)
}

// nfsPathFor gives the export path to look for: --nfs-path, else the
// mountpoint of the dataset, else /<dataset> (with a warning).
func (o *esxiOpts) nfsPathFor(dataset string) (string, string) {
	if o.nfsPath != "" {
		return o.nfsPath, ""
	}
	if dataset == "" {
		return "", "no --nfs-path and no --dataset"
	}
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", dataset).Output()
	if err == nil {
		mp := strings.TrimSpace(string(out))
		if strings.HasPrefix(mp, "/") {
			return mp, ""
		}
	}
	return "/" + dataset, fmt.Sprintf("could not read the mountpoint of %s (no local zfs, or none/legacy) - assuming the NFS export is /%s; use --nfs-path if that is wrong", dataset, dataset)
}

// esxiGuests turns discovered VMs into freezer guests.
func esxiGuests(vms []esxi.VM) []freezer.Guest {
	gs := make([]freezer.Guest, 0, len(vms))
	for _, vm := range vms {
		gs = append(gs, freezer.Guest{VMID: vm.ID, Name: vm.Name, Type: freezer.TypeVM, Platform: freezer.PlatformESXi, Dataset: strings.Join(vm.Datastores, ",")})
	}
	return gs
}

func dsNames(ds []esxi.Datastore) []string {
	var n []string
	for _, d := range ds {
		n = append(n, d.Name)
	}
	return n
}

func (o *esxiOpts) discover(ctx context.Context, tr esxi.Transport, dataset string, recursive bool) (*esxi.Discovery, string, error) {
	path, warn := o.nfsPathFor(dataset)
	d, err := esxi.DiscoverNFS(ctx, tr, esxi.DiscoverOpts{Path: path, Recursive: recursive, Server: o.nfsServer, VMs: o.vms, AllowMixed: o.allowMixed, IncludeOff: o.includeOff})
	if err != nil {
		return nil, path, err
	}
	if warn != "" {
		d.Warnings = append([]string{warn}, d.Warnings...)
	}
	return d, path, nil
}

var reZfsName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/ -]*$`)

// takeZFS creates the ZFS snapshot: locally, or with the --zfs-cmd template.
func takeZFS(o *esxiOpts, dataset, name string, recursive bool) error {
	if o.zfsCmd == "" {
		return freezer.Snapshot(dataset, name, recursive)
	}
	for _, v := range []string{dataset, name} {
		if !reZfsName.MatchString(v) || strings.ContainsAny(v, " ") {
			return fmt.Errorf("--zfs-cmd: refusing unsafe dataset/snapshot name %q", v)
		}
	}
	cmdline := strings.NewReplacer("{dataset}", dataset, "{snapshot}", name, "{fullname}", dataset+"@"+name).Replace(o.zfsCmd)
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/C", cmdline)
	} else {
		c = exec.Command("sh", "-c", cmdline)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs-cmd %q: %w (%s)", cmdline, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func collectWarnings(tr esxi.Transport, d *esxi.Discovery, extra ...string) []string {
	var w []string
	w = append(w, extra...)
	if d != nil {
		w = append(w, d.Warnings...)
	}
	if tr != nil {
		w = append(w, tr.Warnings()...)
	}
	// de-duplicate, keep order
	seen := map[string]bool{}
	var out []string
	for _, s := range w {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func esxiTimeout(t time.Duration, set bool) time.Duration {
	if set {
		return t
	}
	return 120 * time.Second // quiesced snapshots often take longer than the 30s Proxmox default
}

func runSnapESXi(o *esxiOpts, result runResult, recursive bool, timeout time.Duration, forceFreeze bool) int {
	result.Hypervisor = "esxi"
	dataset, name := result.Dataset, result.Snapshot

	var (
		tr   esxi.Transport
		disc *esxi.Discovery
		path string
		ferr error
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	tr, ferr = o.connect(true)
	if ferr == nil {
		result.Transport = tr.Name()
		defer tr.Close()
		disc, path, ferr = o.discover(ctx, tr, dataset, recursive)
	}
	cancel()

	if ferr != nil {
		result.Warnings = collectWarnings(tr, nil)
		if forceFreeze {
			return result.fail(fmt.Errorf("--forcefreeze: %w - aborting without snapshotting", ferr))
		}
		// Best effort: the ZFS snapshot is more important than the VM snapshots.
		result.Warnings = append(result.Warnings, "esxi: "+ferr.Error()+" - snapshotting without VM snapshots (crash-consistent)")
		return finishZFS(o, result, dataset, name, recursive, nil)
	}
	result.NFSPath = path
	result.Datastores = dsNames(disc.Datastores)
	result.Skipped = disc.Skipped

	guests := esxiGuests(disc.VMs)
	if len(guests) == 0 {
		result.Note = "no powered-on VMs on this NFS - snapshotting without VM snapshots"
	}
	f, err := freezer.NewESXiFreezer(tr, "", name)
	if err != nil {
		return result.fail(err)
	}
	f.ThawTimeout = o.thawTimeout
	freezer.Register(f)

	start := time.Now()
	fzr := freezer.FreezeAll(guests, timeout)
	defer fzr.Thaw() // safety net: Thaw is idempotent
	result.FreezeMs = time.Since(start).Milliseconds()

	if unfrozen := abortList(fzr.Results(), forceFreeze); len(unfrozen) > 0 {
		fzr.Thaw()
		result.Guests = fzr.Results()
		result.Warnings = collectWarnings(tr, disc)
		return result.fail(abortError(unfrozen, forceFreeze))
	}

	snapErr := takeZFS(o, dataset, name, recursive)
	fzr.Thaw()
	result.Guests = fzr.Results()
	result.Warnings = collectWarnings(tr, disc)
	for _, g := range result.Guests {
		if g.Frozen && !g.Thawed {
			result.Warnings = append(result.Warnings, fmt.Sprintf("vm %d (%s): VM snapshot %s still exists on the ESXi host - run cleanup", g.VMID, g.Name, g.SnapID))
		}
	}
	if snapErr != nil {
		result.Status = "error"
		result.Error = snapErr.Error()
		emit(result)
		return 1
	}
	result.Status = "ok"
	emit(result)
	return 0
}

// finishZFS takes the plain ZFS snapshot when there is nothing to freeze.
func finishZFS(o *esxiOpts, result runResult, dataset, name string, recursive bool, g []freezer.GuestResult) int {
	if err := takeZFS(o, dataset, name, recursive); err != nil {
		result.Status = "error"
		result.Error = err.Error()
		emit(result)
		return 1
	}
	result.Status = "ok"
	result.Guests = g
	emit(result)
	return 0
}

// esxiCmdFlags builds the flag set of discover/freeze/cleanup.
func esxiCmdFlags(cmd string) (*flag.FlagSet, *esxiOpts, *string, *string, *bool, *time.Duration) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dataset := fs.String("dataset", "", "ZFS dataset backing the NFS export")
	name := fs.String("name", "", "ZFS snapshot name (used in the VM snapshot names)")
	recursive := fs.Bool("recursive", true, "include NFS datastores exported from child datasets")
	timeout := fs.Duration("timeout", 120*time.Second, "max time to wait per VM snapshot")
	var o esxiOpts
	o.register(fs, false)
	return fs, &o, dataset, name, recursive, timeout
}

func runDiscoverESXi(args []string) int {
	fs, o, dataset, _, recursive, _ := esxiCmdFlags("discover")
	fs.Parse(args)
	res := runResult{Dataset: *dataset, Hypervisor: "esxi"}
	tr, err := o.connect(false)
	if err != nil {
		return res.fail(err)
	}
	defer tr.Close()
	res.Transport = tr.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, path, err := o.discover(ctx, tr, *dataset, *recursive)
	if err != nil {
		res.Warnings = collectWarnings(tr, nil)
		return res.fail(err)
	}
	res.NFSPath = path
	res.Datastores = dsNames(d.Datastores)
	res.Skipped = d.Skipped
	res.Warnings = collectWarnings(tr, d)
	for _, g := range esxiGuests(d.VMs) {
		res.Guests = append(res.Guests, freezer.GuestResult{VMID: g.VMID, Name: g.Name, Type: g.Type, Platform: g.Platform, Strategy: "would-snapshot"})
	}
	res.Status = "ok"
	emit(res)
	return 0
}

func runFreezeESXi(args []string) int {
	fs, o, dataset, name, recursive, timeout := esxiCmdFlags("freeze")
	fs.Parse(args)
	res := runResult{Dataset: *dataset, Snapshot: *name, Hypervisor: "esxi"}
	if *name == "" || o.state == "" {
		return res.fail(errors.New("freeze needs --name (the ZFS snapshot name) and --state (file for the VM snapshot handles)"))
	}
	tr, err := o.connect(true)
	if err != nil {
		return res.fail(err)
	}
	defer tr.Close()
	res.Transport = tr.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	d, path, err := o.discover(ctx, tr, *dataset, *recursive)
	cancel()
	if err != nil {
		res.Warnings = collectWarnings(tr, nil)
		return res.fail(err)
	}
	res.NFSPath, res.Datastores, res.Skipped = path, dsNames(d.Datastores), d.Skipped

	f, err := freezer.NewESXiFreezer(tr, "", *name)
	if err != nil {
		return res.fail(err)
	}
	freezer.Register(f)
	guests := esxiGuests(d.VMs)
	start := time.Now()
	sess := freezer.FreezeAll(guests, *timeout)
	res.FreezeMs = time.Since(start).Milliseconds()
	res.Guests = sess.Results()
	res.Warnings = collectWarnings(tr, d)
	if bad := abortList(sess.Results(), false); len(bad) > 0 {
		sess.Thaw()
		res.Guests = sess.Results()
		return res.fail(abortError(bad, false))
	}

	// The state file is what makes the snapshots removable later; if it cannot
	// be written, take them off again right away rather than leaking them.
	st := freezer.ESXiState{Created: time.Now().UTC().Format(time.RFC3339), Host: tr.Name(), Dataset: *dataset, Snapshot: *name, Guests: f.State()}
	for i := range st.Guests {
		for _, g := range guests {
			if g.VMID == st.Guests[i].VMID {
				st.Guests[i].Name = g.Name
			}
		}
	}
	if err := freezer.WriteESXiState(o.state, st); err != nil {
		sess.Thaw()
		res.Guests = sess.Results()
		return res.fail(fmt.Errorf("cannot write state file: %w - VM snapshots were removed again", err))
	}
	res.State = o.state
	res.Status = "ok"
	emit(res)
	return 0
}

func runThawESXi(args []string) int {
	fs, o, _, _, _, _ := esxiCmdFlags("thaw")
	fs.Parse(args)
	res := runResult{Hypervisor: "esxi"}
	if o.state == "" {
		return res.fail(errors.New("thaw needs --state (the file written by freeze)"))
	}
	st, err := freezer.ReadESXiState(o.state)
	if err != nil {
		return res.fail(err)
	}
	res.Dataset, res.Snapshot = st.Dataset, st.Snapshot
	tr, err := o.connect(false)
	if err != nil {
		return res.fail(err)
	}
	defer tr.Close()
	res.Transport = tr.Name()

	f, err := freezer.NewESXiFreezer(tr, "", st.Snapshot)
	if err != nil {
		return res.fail(err)
	}
	f.ThawTimeout = o.thawTimeout
	f.Restore(st.Guests)
	freezer.Register(f)
	var guests []freezer.Guest
	for _, g := range st.Guests {
		guests = append(guests, freezer.Guest{VMID: g.VMID, Name: g.Name, Type: freezer.TypeVM, Platform: freezer.PlatformESXi})
	}
	sess := freezer.RestoreSession(guests)
	sess.Thaw()
	res.Guests = sess.Results()
	allOK := true
	for _, g := range res.Guests {
		if !g.Thawed {
			allOK = false
		}
	}
	res.Warnings = collectWarnings(tr, nil)
	if !allOK {
		res.Status, res.Error = "error", "not all VM snapshots could be removed - the state file was kept, retry thaw or run cleanup"
		emit(res)
		return 1
	}
	if err := os.Remove(o.state); err != nil {
		res.Warnings = append(res.Warnings, "could not remove state file: "+err.Error())
	}
	res.Status = "ok"
	emit(res)
	return 0
}

func runCleanupESXi(args []string) int {
	fs, o, dataset, _, recursive, _ := esxiCmdFlags("cleanup")
	fs.Parse(args)
	res := runResult{Dataset: *dataset, Hypervisor: "esxi"}
	tr, err := o.connect(false)
	if err != nil {
		return res.fail(err)
	}
	defer tr.Close()
	res.Transport = tr.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d, path, err := o.discover(ctx, tr, *dataset, *recursive)
	if err != nil {
		return res.fail(err)
	}
	res.NFSPath, res.Datastores = path, dsNames(d.Datastores)

	// Every VM on the NFS, powered on or not, may hold one of our snapshots.
	vms := d.OnNFS
	removed, errs := freezer.CleanupESXi(ctx, tr, vms)
	res.Removed = removed
	res.Warnings = collectWarnings(tr, d, errs...)
	if len(errs) > 0 {
		res.Status, res.Error = "error", fmt.Sprintf("%d snapshot(s) could not be removed", len(errs))
		emit(res)
		return 1
	}
	res.Status = "ok"
	emit(res)
	return 0
}
