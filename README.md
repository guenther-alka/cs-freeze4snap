# cs-freeze4snap

Consistent ZFS snapshots for Proxmox VM/LXC guests and ESXi VMs - freeze if possible,
snapshot regardless.

Part of the [napp-it 4ai (client-server edition)](https://napp-it.org) cluster tooling family
(alongside [cs-tools](https://www.napp-it.org/cs-tools_en.html))
csweb-gui deploys and updates this manually per member menu About > Download cs-tools

## Goal

ZFS's copy-on-write design with atomic transaction-group (txg) commits
means `zfs snapshot` **can never corrupt the pool itself** - that
guarantee is absolute, not "usually" or "for most filesystems". A
snapshot taken mid-write is always a valid, mountable point-in-time
image of whatever was on disk at that instant.

But that guarantee only covers what ZFS itself can see: the raw bytes
on the zvol/dataset. **It says nothing about whether those bytes form
a consistent filesystem from the perspective of an OS running inside a
VM.** For a VM's virtual disk, a snapshot taken while the guest is
writing is - from the guest's own point of view - exactly equivalent
to someone pulling the power cord: whatever was on disk at that
instant is what the guest sees on next boot, no more, no less. Whether
that's safe depends entirely on how well the *guest's own* filesystem
recovers from an unclean shutdown - modern journaled filesystems
(ext4, XFS, NTFS, ...) handle this well via their own journal replay;
older or non-journaled filesystems may not.

`cs-freeze4snap` exists specifically to reduce this second, guest-side
risk - ZFS can't do anything about it on its own, since it has no
visibility into what's happening inside a running VM. Freezing pauses
guest I/O (or the whole guest) right before the snapshot, so the
captured state is a clean stopping point rather than an arbitrary
mid-write instant - **minimizing** that risk, not eliminating it
entirely (see [Residual risk even with freeze](#residual-risk-even-with-freeze)
below). This happens **without ever blocking the snapshot from
happening** - the guiding principle, in the project owner's words:

> take the ZFS snapshot with a VM freeze if at all possible, and do it
> as well as it can be done.

Concretely: freeze every guest it can, by the best method available for
that guest, take the snapshot, thaw everything again - and if a guest can't
be frozen at all for whatever reason, snapshot it anyway rather than
aborting the whole job.

## Quick example

```
cs-freeze4snap snap --dataset rpool/data --name 20260809_1530
```

```json
{"dataset":"rpool/data","snapshot":"20260809_1530","status":"ok","freeze_ms":6,
 "guests":[
   {"vmid":100,"type":"vm","platform":"proxmox-qemu","frozen":true,"strategy":"qga","thawed":true},
   {"vmid":101,"type":"vm","platform":"proxmox-qemu","frozen":true,"strategy":"qmp-pause","thawed":true},
   {"vmid":103,"type":"lxc","platform":"proxmox-lxc","frozen":true,"strategy":"cgroup","thawed":true}
 ]}
```

## Options

```
cs-freeze4snap snap --dataset <ds> --name <snapname> [options]

  --dataset string        ZFS dataset to snapshot (required), e.g. rpool/data
  --name string            Snapshot name (required), e.g. 20260809_1530
  --recursive               Pass -r to zfs snapshot (default: true)
  --exclude string          Comma-separated VMIDs to skip freezing
  --include-only string     Comma-separated VMIDs; overrides discovery entirely
  --timeout duration        Max time to wait per guest freeze (default 30s)
  --forcefreeze             Abort WITHOUT snapshotting if any guest could
                              not be cleanly frozen (default: false)
  --policy 'chain'          Freeze chain, repeatable (see "Freeze chains"), or
                              'memkeep=N' (cap of Proxmox memory snapshots per VM)
  --pre-snap-cmd 'cmd'      Shell command run after the freeze, right before the
                              ZFS snapshot (v1.3.0; failure = warning only)
```

Guest discovery is automatic: `cs-freeze4snap` runs `zfs list -r <dataset>`
and matches Proxmox's own storage-naming convention (`vm-<id>-disk-*` for
QEMU/KVM, `subvol-<id>-disk-*` for LXC) to figure out which guests live on
the dataset - no VMID list to maintain by hand. `--exclude`/`--include-only`
are there for the exceptional case where the default discovery isn't what
you want for a given job.

Result is a single JSON object on stdout, meant to be parsed by the caller
(e.g. `job-snap.pl`/`job-prox_snap.pl`) rather than read by a human - see
[Result format](#result-format) below.

## How the freeze cascade works

This is the core design decision in the tool, so it's worth spelling out in
detail. For each discovered guest, `cs-freeze4snap` tries progressively
weaker - but progressively more universally available - freeze strategies,
and only gives up (gracefully) once none of them work.

### Multiple guests are frozen in parallel, not one after another

When a dataset holds several VMs/containers, all of them are discovered
and frozen **concurrently** (via goroutines), not sequentially one at a
time. The tool waits until *every* guest has either frozen successfully
or been determined unfreezable, and only then fires a single
`zfs snapshot -r` covering all of them at once - then thaws everyone in
parallel again.

```
 discover guests on dataset
          │
          ▼
 freeze VM-A ─┐
 freeze VM-B ─┼─▶  wait for all  ─▶  ONE zfs snapshot -r  ─▶  thaw all
 freeze LXC-C ─┘        (parallel)         (atomic)              (parallel)
```

This matters: freezing guests one at a time would leave a window where
the first guest is already paused while later guests are still writing
right up until the (single, shared) snapshot moment - undermining the
whole point of freezing if the guests interact with each other or share
external state. Parallel freeze/thaw means every guest under the dataset
pauses for approximately the same instant, and the snapshot captures that
shared instant for all of them. In practice this keeps the total freeze
window short even with several guests - the slowest guest determines how
long the freeze phase takes, not the sum of all guests (in live testing
against a real Proxmox host this stayed in the low-millisecond range even
with a mix of VM/LXC guests using different freeze strategies).

### VMs (QEMU/KVM under Proxmox)

> **Since v1.3.0** the built-in chain of a Proxmox VM is `freeze,memory,zfs`: stage 1 below, then - instead of the
> QMP pause - a `qm snapshot` with the RAM state (see "Proxmox VMs: the memory step"), then the plain ZFS
> snapshot. The QMP stop/cont pause (stage 2) is still available as the `pause` step (`[freeze,pause,zfs]`) and is
> what v1.0.0 - v1.2.0 did by default.

```
 1. QEMU Guest Agent (QGA) fsfreeze
    ├─ requires qemu-guest-agent installed & running inside the guest
    ├─ pauses only writes; the guest OS keeps running and flushes its own
    │  journal first - the best case, filesystem-consistent
    └─ if the agent isn't configured/running/responding → fall through

 2. QMP stop / cont
    ├─ the hypervisor control socket that exists for every running
    │  QEMU/KVM VM regardless of guest OS or in-guest tooling
    ├─ pauses ALL vCPU execution, not just writes - heavier (network
    │  connections etc. also pause briefly) but needs zero guest
    │  cooperation and works on literally any guest OS QEMU can boot
    └─ if even the QMP socket is unreachable (e.g. VM not actually
       running) → fall through

 3. None - snapshot as-is
    ├─ strategy "none", frozen: false, but Freeze() returns NO error
    ├─ the snapshot proceeds regardless - crash-consistent for this
    │  guest, same as a real power-loss recovery point
    └─ this is the expected, common outcome for VMs that simply never had
       a guest agent installed - not a failure condition
```

Each stage is attempted with its own connection + protocol handshake
(QGA and QMP both speak newline-delimited JSON-RPC over a unix socket,
`/var/run/qemu-server/<vmid>.qga` and `.qmp` respectively - implemented
directly in this tool, no external client library). A short `guest-ping`
(QGA) or the mandatory `qmp_capabilities` handshake (QMP) is done first, so
a genuinely unresponsive guest fails fast rather than eating the whole
`--timeout` before falling through to the next stage.

### LXC containers

```
 1. Host-side fsfreeze (FIFREEZE ioctl)
    ├─ applied directly to the container's ZFS mountpoint from the HOST,
    │  not via `pct exec` inside the container
    ├─ this matters specifically for *unprivileged* containers: their
    │  rootfs mount is owned by the host's user namespace, not the
    │  container's, so `pct exec ... fsfreeze` fails with EPERM even as
    │  root inside the container. Running fsfreeze as a normal host
    │  operation on a normal host mount sidesteps that entirely - works
    │  identically for privileged and unprivileged containers
    ├─ forces a journal flush, blocks new writes at the VFS layer -
    │  strongest guarantee available for LXC
    └─ not guaranteed to be supported - see Test results below

 2. sync + cgroup v2 freeze
    ├─ writes "1" to the container's cgroup.freeze, which pauses every
    │  process in the container (not just writes - closer in spirit to
    │  QMP stop than to fsfreeze)
    ├─ does NOT force a flush by itself, so an explicit sync of the
    │  mountpoint is issued first
    └─ this is the fallback when FIFREEZE isn't supported by the
       underlying filesystem (see Test results)
```

There is currently no "give up gracefully" third stage for LXC the way
there is for VMs, because sync+cgroup-freeze is already close to
universally available on any Linux host with cgroup v2 - if that write
itself fails, something is unusual enough (missing cgroup delegation,
read-only cgroupfs, ...) that it's reported as a Warning like everything
else, and the snapshot still proceeds as-is.

### Residual risk even with freeze

Freeze **minimizes** the guest-side risk described in [Goal](#goal); it
does not make it disappear entirely. A few narrow, real cases remain:

- **The guest's own filesystem may not support freezing cleanly.** This
  is the same class of problem this project already hit on the *host*
  side (ZFS not supporting `FIFREEZE` - see Test results below), just
  one layer deeper: if a guest kernel's driver for whatever filesystem
  is mounted doesn't implement freeze support (rare, mostly very old or
  exotic filesystems), `guest-fsfreeze-freeze` either fails outright
  (this tool falls through to QMP pause in that case) or, in principle,
  could no-op silently on a sufficiently old/broken guest kernel.
- **Freeze doesn't force applications to persist unwritten state.** It
  guarantees that whatever an application *has already written* lands
  atomically and completely - it doesn't make an application write
  anything it hadn't already decided to. An in-memory cache that hasn't
  checkpointed yet is gone either way, freeze or not - that's an
  application design question, not something storage-level tooling can
  fix.
- **QMP pause is, if anything, the stronger of the two active
  strategies**, not the weaker one: it halts all vCPU execution
  entirely (nothing can start a new write), while QEMU's block layer
  still drains any I/O that was already in flight before the pause
  takes effect - eliminating torn-write risk essentially completely.
  QGA fsfreeze is solid too, but its guarantee is only as good as the
  guest kernel's own freeze implementation for the specific filesystem
  involved (see the first bullet above).

None of this is a reason not to freeze - it reduces the guest-side risk
window from "any write, any time" to "a small set of edge cases", which
is a large practical improvement. It's just not an absolute guarantee,
the same way the host-level ZFS guarantee is.

**Aside: what about running ZFS as the guest filesystem too?** A VM
whose own filesystem is ZFS (e.g. an OmniOS/FreeBSD/Linux guest with a
ZFS pool on the virtual disk) gets the same COW/atomic-txg guarantee
one layer down, independent of the host - a snapshot mid-write is,
again, always a valid point-in-time image, no journal replay needed.
That's a genuine, structural edge over journaled filesystems even
*without* freeze - the tradeoff is real too, though: double
copy-on-write, double checksumming, and generally more resource
overhead than a single-layer filesystem. **With freeze in the picture,
that edge narrows substantially.** QMP pause in particular halts guest
I/O regardless of the guest's filesystem - so a journaled guest FS
(ext4, XFS, NTFS) freezes essentially as cleanly as ZFS-on-ZFS would.
What's left is a narrow structural difference outside the freeze
window itself: ZFS's COW guarantee applies to every write during
normal operation, not just the frozen instant, where a journaled FS
could in principle (rarely) still hit a state needing journal replay
from ordinary operation. In short: ZFS-in-guest without freeze is a
real crash-safety-for-resource-overhead tradeoff; with freeze, a
journaled guest filesystem gets close enough to the same safety that
the choice becomes more about the guest FS's own feature set than
about consistency risk.

### Never a hard failure

Regardless of platform, **no freeze problem ever aborts the job** by
default: a guest that can't be frozen gets `frozen: false` and a `warning`
field explaining why, and the ZFS snapshot is taken anyway. This applies
uniformly whether the cause is "no guest agent configured" (the common
case), "Proxmox/QMP didn't respond" (unusual, possibly worth investigating,
but still not snapshot-blocking), or any other freeze-side error.

For the cases where you'd rather know *before* a bad snapshot lands, use
`--forcefreeze`: if any guest ends up with `frozen: false`, the whole job
aborts (after thawing whatever *did* freeze) without taking the snapshot
at all, and exits non-zero.

### Cross-pool disk warning

`zfs snapshot -r` never crosses pool boundaries. If a guest has disks on
more than one pool (common for VMs that mix a fast NVMe pool for the OS
disk with a bulk `rpool` for data disks), only the disk(s) under the
targeted dataset's pool end up in the snapshot - the others are silently
untouched by `-r`, no matter how deep the recursion. `cs-freeze4snap` runs
an additional pool-wide `zfs list` after discovery specifically to catch
this and surface it as a `warnings` entry, e.g.:

```
vmid 101 (vm) has disk(s) outside rpool/data that will NOT be included
in this snapshot: nvme480/vm-101-disk-0
```

This check is itself best-effort/advisory: if it can't run (e.g.
permissions), that's logged but doesn't affect the snapshot either.

## Recommended: combine with napp-it CS's config-include feature

If you're driving this tool from [napp-it CS](https://napp-it.org)'s
`job-snap.pl` (see `--recursive` above), there's a genuinely useful
combination worth calling out: napp-it CS's snap jobs have their own
`include=<folders>` feature that syncs regular (non-ZFS) folders into a
`_include` subfolder of the target dataset *before* the snapshot is
taken. Pointed at Proxmox's cluster-wide `/etc/pve` (which holds every
VM/CT's `.conf` file), the execution order becomes:

```
1. include  → /etc/pve synced into <dataset>/_include/_etc_pve
2. freeze   → this tool's cascade (QGA/QMP/cgroup) pauses the guests
3. snapshot → one atomic, recursive zfs snapshot -r captures BOTH
```

Since v1.3.0, with the Proxmox `memory` step the config is synced **once more** between freeze and snapshot
(`--pre-snap-cmd`, see "Proxmox VMs: the memory step"): the memory snapshot adds a section to the VM's `.conf`,
and only the copy taken after it lets `qm rollback` work after a restore. napp-it CS does this by itself and
adds `/etc/pve` to `include=` automatically for `freeze=proxmox`.

The result: a single snapshot holding the VM/CT **configuration**
(CPU, RAM, disks, network - whatever was in `/etc/pve` right before the
freeze window) together with **consistently frozen disk data** for
every guest under the dataset. Useful for restores where the config
might have drifted from what's assumed (extra disk added, RAM resized,
...) since the last time anyone looked.

This only pays off with a **recursive** snapshot covering a whole
multi-guest dataset in one pass - a non-recursive snapshot (or,
elsewhere in the napp-it CS ecosystem, several separate non-recursive
jobs each covering a single zvol of a multi-disk VM) freezes/snapshots
independently each time, so disks captured in different runs are not
consistent with each other even though each is individually consistent.
`/etc/pve` itself is cluster-wide, so even guests whose disks live
outside this particular dataset still get their current config
captured by the include step, just without matching frozen disk data
from this specific run.

Verified end-to-end (see Test results below): a recursive snap job with
`include=/etc/pve` synced real VM/CT config files into
`<dataset>/_include/_etc_pve` before freezing, and the resulting
snapshot was confirmed (via `.zfs/snapshot/.../`) to contain those
config files alongside the frozen guest disk data.

## ESXi: hotsnap of all VMs on an NFS datastore (v1.1.0, chains and auto protocol v1.2.0)

`cs-freeze4snap` can also take the ZFS snapshot of an **NFS export used as an
ESXi datastore** in a VM-consistent way. It finds the VMs on that NFS itself,
takes a VM snapshot of each powered-on VM on the ESXi host (the "freeze"), takes
the ZFS snapshot, and removes the VM snapshots again (the "thaw"). The ZFS
snapshot then holds every VM with an embedded ESXi snapshot: after a restore
(`zfs clone`/`rollback`) revert the VM to that snapshot to get a
filesystem-consistent (`freeze`) or running-state (`mem`) image instead of a
crash image.

```
cs-freeze4snap snap --hypervisor esxi --cfg /path/esxi.cfg \
    --dataset tank/nfs --name auto_20260919_2100 --storage nfs --vms all
```

Because the ESXi host is reached over the network (ssh or soap), the tool does
**not** have to run on the ZFS server - any machine that can reach the ESXi host
works (a frontend, a jump host). The ZFS snapshot is taken by the local `zfs`, or
on a remote machine by `--zfs-cmd`:

```
cs-freeze4snap snap --hypervisor esxi --cfg esxi.cfg --dataset tank/nfs --name s1 \
    --zfs-cmd 'ssh root@nas zfs snapshot -r {fullname}'
```

`{dataset}`, `{snapshot}` and `{fullname}` (`dataset@snapshot`) are replaced; the
names are checked against `[A-Za-z0-9_.:/-]` first. The command runs via `sh -c`
(`cmd /C` on Windows).

### How the VMs are found

1. The NFS export path comes from `--nfs-path`, or from the `mountpoint` of
   `--dataset` (`zfs get mountpoint`; without a local zfs it assumes `/<dataset>`
   and warns).
2. The host's NFS datastores whose export matches (plus exports of child
   datasets with `--recursive`, the default) are selected. If the same path is
   exported by more than one NFS server, name it with `--nfs-server`.
3. Every VM with files on those datastores is a candidate. Selected for a VM
   snapshot are the powered-on VMs (`--vms all`, or a comma list of ids and names).
   Skipped, and listed with the reason in `skipped`:
   - powered off / suspended VMs - they are consistent anyway (`--include-off` snapshots them too),
   - VMs that also have disks on another datastore - a ZFS snapshot of this NFS would hold only part
     of them (`--allow-mixed` to snapshot them anyway, with a warning),
   - VMs not in the `--vms` list.

`discover` shows exactly that without changing anything:

```
cs-freeze4snap discover --cfg esxi.cfg --nfs-path /tank/nfs
```

### Connection: `--cfg`

**Host table** (one file for all servers, one server per line, `#` comments):

```
# host,user,password
192.168.2.48,root,secret
192.168.2.49,root,pass,with,commas    # the password is everything after the 2nd comma
192.168.2.50,cert                     # ssh key of the running user (~/.ssh/id_ed25519, id_ecdsa, id_rsa), user root
192.168.2.51,ops,cert                 # same key, other user
192.168.2.52,root,cert,/etc/keys/esx  # explicit OpenSSH private key
```

`--host <ip>` selects the line (with only one line in the file it can be left out).
`cert` entries work with ssh only (OpenSSH keys, no PuTTY `.ppk`); soap needs a password.
The same file can serve several tools and jobs. Options that belong to one server are extra lines
`<host>:<option>=<value>` in the same file (v1.2.0): `proto`, `port`, `hostkey`, `tls_sha256`,
`timeout`, `useragent`, e.g.

```
192.168.2.48:proto=ssh
192.168.2.48:hostkey=SHA256:abcd...
```

**Single-server key=value file** - use it when a server needs more than login data:

```
host=192.168.2.48
user=root
password=...              # or CS_ESXI_PASSWORD in the environment - never a command line flag
proto=auto                # auto (default: soap, ssh if soap cannot be reached), ssh (vim-cmd) or soap (vSphere API, port 443)
hostkey=SHA256:...        # ssh: pin the host key (the first run prints it in "warnings")
tls_sha256=...            # soap: pin the certificate (sha256 hex)
key=/path/id_ed25519      # ssh: OpenSSH private key instead of a password (no PuTTY .ppk)
timeout=15                # connect timeout in seconds
```

The format is detected from the first entry line (`host,...` is a table, `key=...` a key=value file).
`CS_ESXI_HOST`, `CS_ESXI_USER` and `CS_ESXI_PASSWORD` override the file;
`--host`, `--user`, `--proto` override both. Keep the file readable by the
job user only (`chmod 600`). Without `hostkey`/`tls_sha256` (only possible in the key=value form) the
connection works but the JSON `warnings` tell you what to pin.

| | ssh | soap |
|---|---|---|
| free ESXi license | works (`vim-cmd`) | works: the free license only accepts the write calls from a client whose User-Agent starts with "VMware" - the tool sends `VMware VI Client/4.0.0` (`useragent=` overrides it) |
| speed (16 VMs, measured) | ~12 s to list VMs, ~3 s per snapshot | ~0.2 s to list VMs, ~2 s per snapshot |
| needs | ssh service on the host | port 443 |

**`--proto auto`** (default since v1.2.0) tries soap first and falls back to ssh when the vSphere API
cannot be reached (a warning says so). A failed soap *login* is final and not retried over ssh: the same
credentials would fail again and every failed login counts towards the account lockout of the ESXi host
(5 failures by default). Entries with `cert` use ssh directly. Use `--proto ssh|soap` or `<host>:proto=` to
force one. The ESXi
clock is not used (the tool names snapshots itself), so a wrong host clock does no harm.

### Freeze chains (v1.2.0, Proxmox memory step and new defaults v1.3.0)

What "freezing" a VM means is a **chain** of steps that are tried from left to right; the first
step that works wins, the ZFS snapshot is taken afterwards. Every step has its own timeout.

| step | ESXi | Proxmox VM | Proxmox LXC |
|---|---|---|---|
| `freeze` (alias `quiesce`) | VM snapshot, guest filesystem quiesced by VMware Tools | QEMU guest agent `fsfreeze` | `fsfreeze`, else cgroup freeze |
| `memory` (alias `mem`) | VM snapshot including the RAM state (hot snapshot, slower; restoring it resumes the running state) | `qm snapshot --vmstate` with the RAM state, kept as long as the ZFS snapshot exists (v1.3.0, see below) | - |
| `plain` | VM snapshot of the disks only (crash-consistent) | - | - |
| `pause` | - | QMP `stop`/`cont` (the whole VM is paused) | - |
| `zfs` | give up freezing, take the ZFS snapshot as it is | same | same |

Steps that do not exist on a platform are skipped, so one global chain can serve ESXi and Proxmox.
A number is a timeout in seconds: `memory:300` for one step, a bare number for every step without its
own. Without a timeout the `--timeout` value applies (30 s, ESXi 120 s).

Chains live in the cfg file next to the server list (or in `--policy`):

```
[freeze,memory,zfs,30]               # global default: freeze, else memory, else ZFS only; 30 s per step
192.168.2.48:*,freeze,plain,zfs      # every VM of this ESXi host
192.168.2.48:vm100,memory,zfs        # one VM (vm100, ct100 or 100)
192.168.2.203:vm101,freeze,memory    # Proxmox member, no zfs at the end -> strict
```

Precedence (strongest first): `--mode`, `--policy`, `host:vmid` line, `host:*` line, global `[...]`,
built-in default (**v1.3.0:** ESXi and Proxmox VM `freeze,memory,zfs`, LXC `freeze,zfs`; up to v1.2.0 ESXi was
`quiesce,plain,zfs` and Proxmox VM `quiesce,pause,zfs`).
`--mode freeze|mem|plain` stays as a shortcut for one chain for all VMs (`freeze,plain,zfs` /
`memory,plain,zfs` / `plain,zfs`). `--policy` (repeatable) takes the same lines without the host prefix:
`--policy '[freeze,memory,zfs,30]' --policy 'vm100,memory,zfs'`.

The step was called `quiesce` up to v1.2.0 (VMware wording); `freeze` is the name Proxmox/QEMU use
(`fsfreeze`). `quiesce` is still accepted everywhere as an alias and is written back as `freeze`.

Why `freeze,memory,zfs`: `freeze` gives a filesystem-consistent point when the guest tools work; when they
do not (no VMware Tools / no QEMU guest agent) the `memory` snapshot still gives a point that can be
restored safely, because the RAM state is part of it. `plain` (ESXi) and `pause` (Proxmox) only give
crash-consistent points and have to be asked for. `memory,freeze,zfs` puts the hot snapshot first (every
restore point is a running-state point). `freeze,zfs` alone leaves restore points that are only as
consistent as the guest tools allowed.

A Proxmox VM that is **not running** needs no freeze: it is reported as frozen with strategy `stopped` (a
note says so) and does not break a strict chain.

**Best effort or strict.** A chain that ends with `zfs` is best effort as before: when every step failed
the guest is reported not frozen (`guests[].warning`) and the ZFS snapshot is taken anyway. A chain
**without** `zfs` is strict for its guests: if no step works the run aborts *before* the ZFS snapshot
(exit 1, `status: error`), like `--forcefreeze` but per guest. A chain of only `zfs` takes no VM snapshot
at all for that guest (strategy `zfs-only`).

**Timeouts.** A step that runs out of time counts as failed and the next step starts. With soap the tool
cancels the vSphere task first (`CancelTask`) and waits until the host has given it up, otherwise the host
would keep writing the snapshot and answer the next step with "Another task is already in progress". With
ssh a running `vim-cmd` cannot be cancelled from the outside: the tool removes a snapshot the host may still
create for the failed step, and if the host is still busy the following step fails too - `cleanup` removes
what is left. Prefer soap (`--proto auto` does) when you rely on step timeouts. For Proxmox the timeout
bounds each connection/command of the step. The JSON result shows the chain that applied in
`guests[].chain`.

As with Proxmox, a VM that cannot be snapshotted never blocks the ZFS snapshot (unless strict) -
it is reported in `warnings`. An unreachable ESXi host or a wrong password is treated the same way:
the ZFS snapshot is taken without VM snapshots and the reason is in `warnings` (with `--forcefreeze` the run
aborts).

The VM snapshots are named `cs4s-<snapshot>-<vmid>` and carry the description
`cs-freeze4snap`; only such snapshots are ever removed by the tool.

### Proxmox VMs: the `memory` step (v1.3.0)

`freeze` needs the QEMU guest agent inside the guest. Without it (no agent installed, BSD/appliance guests) the
default chain `freeze,memory,zfs` falls back to `memory`: the tool runs

```
qm snapshot <vmid> cs4s_<snapshot> --vmstate 1 --description "cs-freeze4snap <snapshot>"
```

which needs **no guest agent and no guest tools**: `qm` stops the VM for a moment (seconds, depends on the
RAM size - 4 s for 2 GB in the test), writes the RAM into a `vm-<id>-state-...` volume and snapshots the disks
at that same point. Restoring it gives a running VM in exactly that state, not a crash.

Differences to ESXi, on purpose:

- The VM snapshot is **not removed** after the ZFS snapshot. On ZFS the consistent state is the pair
  `vm-<id>-disk-N@cs4s_<snapshot>` + the vmstate volume; removing the VM snapshot would destroy exactly that.
  The recursive ZFS snapshot (`pool@<snapshot>`) is taken a moment *later* and holds the disks slightly newer
  than the RAM - the RAM state fits the `cs4s_` snapshot, not the later disk state.
- The VM snapshot lives as long as the ZFS snapshot named in its description: at the start of every run the
  tool removes the `cs4s_` snapshots of a VM whose ZFS snapshot is gone (the retention of your snapshot job
  removed it), with a grace period of 10 minutes for parallel jobs. Only snapshots with the name prefix `cs4s_`
  **and** the description `cs-freeze4snap ...` are ever touched. A snapshot that is the base of a running
  restore (its RAM state volume still carries the ZFS snapshot) is kept.
- `memkeep=N` (global line, `host:memkeep=N` for one Proxmox member, or `--policy 'memkeep=N'`) is an optional
  cap: at most the newest N memory snapshots per VM are kept, even if their ZFS snapshots still exist
  (0 = no cap, the default). Proxmox reserves the vmstate volume thick - about 2.2 x the RAM size (4.5 GB for a
  2 GB VM) - so for RAM-heavy VMs with long retention set `memkeep` (e.g. 3), otherwise retention x RAM is
  reserved in the pool.
- A VM that is **not running** counts as consistent: strategy `stopped`, no snapshot is taken.
- A failed or timed-out `qm snapshot` is removed again (a timed-out one cannot be cancelled cleanly, the Proxmox
  worker may still finish - the leftover is removed by the next run). The default timeout of the step is 120 s.

**The VM configuration must be part of the ZFS snapshot**, because the memory snapshot is a section of
`/etc/pve/qemu-server/<id>.conf` and `qm rollback` needs it after a restore. The napp-it CS jobs add `/etc/pve`
to `include=` automatically for `freeze=proxmox` (see below). When you call the tool yourself, sync it with

```
cs-freeze4snap snap --dataset tank/vm --name s1 \
    --pre-snap-cmd 'rsync -a --delete /etc/pve/ /tank/vm/_include/_etc_pve'
```

`--pre-snap-cmd` runs after the freeze and right before `zfs snapshot` (a normal include sync before the freeze
would miss the new snapshot section of the config). It runs with `sh -c`, 120 s at most; a failure is only a
warning in the result.

**Restore** (VM stopped, disks and config back from the ZFS snapshot / replication target):

```
zfs rollback -r tank/vm/vm-100-disk-0@cs4s_<snapshot>    # every disk of the VM
qm rollback 100 cs4s_<snapshot>                          # RAM state, VM comes up running
```

The `zfs rollback -r` first is required: Proxmox refuses `qm rollback` while a newer ZFS snapshot (the
recursive `@<snapshot>`, later job snapshots) exists on the disk. It only removes ZFS snapshots that are newer
than the memory snapshot. If the config is gone (new host), copy it back from
`<dataset>/_include/_etc_pve/qemu-server/<id>.conf` first.

The result of a VM that got a memory snapshot has `strategy: "qm-mem"`, `frozen: true` and a `warning` that
tells which step failed before and the name of the VM snapshot.

### Use from napp-it CS jobs

napp-it CS (snap and replication jobs) has one job setting **Freeze VM prior snap** = `off | proxmox | esxi`.
`proxmox` runs the tool on the ZFS member (Proxmox host); `esxi` runs it on the napp-it CS **frontend** (any
machine that reaches the ESXi host) and takes the ZFS snapshot on the member between freeze and thaw. The
ESXi hosts, logins, protocol (`--proto auto`) and the freeze chains all come from the cfg file
`_cfg/freeze4snap/freeze4snap.cfg` on the frontend (`_cfg/freeze4snap/freeze4snap.readme` describes it). A failed
freeze never blocks the job unless a chain is strict; a missing tool or cfg is a hard error.
For `proxmox` the jobs also make sure that `/etc/pve` is part of the job's `include=` (added when missing; `include=` then
only names *additional* folders) and pass `--pre-snap-cmd` (re-sync of `/etc/pve` right before the ZFS snapshot) and
`memkeep=` from the cfg to the tool.

### Freeze and thaw as separate steps

For jobs where the snapshot is taken by something else (a replication step, a script on
the ZFS server):

```
cs-freeze4snap freeze --cfg esxi.cfg --dataset tank/nfs --name s1 --state /var/tmp/s1.json
zfs snapshot -r tank/nfs@s1                                    # by any means
cs-freeze4snap thaw   --cfg esxi.cfg --state /var/tmp/s1.json
```

`freeze` writes the VM snapshot handles to the state file (mode 0600) and leaves the
snapshots in place; `thaw` removes them and deletes the state file (it keeps the file and exits
non-zero if a snapshot could not be removed; a snapshot that is already gone counts as removed).
**Do not leave VM snapshots standing** - they grow. After a crash run
`cleanup` - it removes every VM snapshot with the tool's tag on the NFS's VMs:

```
cs-freeze4snap cleanup --cfg esxi.cfg --nfs-path /tank/nfs
```

### Extra JSON fields (ESXi)

`hypervisor` (`"esxi"`), `transport`, `nfs_path`, `datastores` (the matched ESXi datastore
names), `skipped` (`vmid`, `name`, `reason`), `state` (`freeze`), `removed` (`cleanup`);
each `guests[]` entry has `platform: "esxi"`, `name`, `strategy` (`esxi-quiesce`,
`esxi-mem`, `esxi-snap`) and `snap_id`. `CS_ESXI_DEBUG=1` prints every ssh command / soap call
with its duration to stderr.

### Restoring

Restore the VM files from the ZFS snapshot (e.g. clone the snapshot and register/copy the VM
folder, or `zfs rollback`), then on the ESXi host revert the VM to the embedded
snapshot (`vim-cmd vmsvc/snapshot.revert <vmid> <snapid> 0`, or in the UI: Snapshots ->
`cs4s-<snapshot>-<vmid>` -> Restore).

## Result format

```jsonc
{
  "dataset": "rpool/data",
  "snapshot": "20260809_1530",
  "status": "ok",              // or "error" - see "error" field
  "error": "...",              // only present if status is "error"
  "note": "...",               // e.g. "no guests discovered"
  "warnings": ["..."],         // cross-pool disks, non-fatal check failures
  "freeze_ms": 6,              // wall-clock time spent in the freeze phase
  "guests": [
    {
      "vmid": 101,
      "type": "vm",            // "vm" | "lxc"
      "platform": "proxmox-qemu",
      "frozen": true,
      "strategy": "qmp-pause", // "qga" | "qmp-pause" | "qm-mem" | "stopped" | "fsfreeze" | "cgroup" | "zfs-only" | "none"
      "thawed": true,
      "warning": "..."         // only present if this guest wasn't cleanly frozen
    }
  ]
}
```

Exit code is `0` for `status: "ok"`, non-zero otherwise (including when
`--forcefreeze` aborts the job). The `zfs snapshot` command itself failing
(permissions, name collision, pool full, ...) is the one thing that *does*
produce `status: "error"` - freeze problems never do.

## Extending to other hypervisors

The freeze logic is behind a small `Freezer` interface with a global
registry (`freezer/freezer.go`), specifically so Proxmox isn't baked into
the orchestration logic:

```go
type Freezer interface {
    Name() string
    Supports(g Guest) bool
    Freeze(g Guest, timeout time.Duration) (strategy string, err error)
    Thaw(g Guest) error
}
```

`main.go` and the discovery/session code never reference QEMU, LXC, or
Proxmox directly - they only ever go through `freezer.For(guest)` to find
whichever registered `Freezer` claims to `Support()` a given guest, then
call `Freeze`/`Thaw` on it. Adding a new platform is:

```go
// freezer/hyperv.go
func init() { Register(&HyperVFreezer{}) }
```

...and nothing else changes. Two platforms are anticipated but **not yet
implemented** (`freezer.PlatformHyperV`, `freezer.PlatformBhyve` constants
exist as placeholders):

**Hyper-V** would most likely mirror the VM cascade conceptually (VSS via
the Hyper-V Integration Services / Data Exchange channel as the strong
case, falling back to a plain `Suspend-VM`/`Resume-VM` pause as the
QMP-equivalent universal fallback) - but the actual API surface is COM-
based and meaningfully more involved than QEMU's plain socket protocols,
so this needs its own implementation effort, not just a config flag.

**bhyve** (illumos) has no built-in guest-communication channel at all
comparable to QGA - there's nothing to "discover" the way `/var/run/
qemu-server/*.qga` exists for free on Proxmox. A `bhyve` `Freezer` would
need its own lightweight guest-side agent (following the same pattern as
this project's sibling tools, `cs-sync`/`cs-stream`: a small Go binary
running inside the guest, reachable over a dedicated channel with
`--allow-ip`), which is a bigger lift than the QEMU/LXC case and likely a
separate project on its own.

## Requirements

- ESXi mode (v1.1.0): runs on **any machine** that reaches the ESXi host over ssh (port 22) or soap
  (port 443); the ZFS snapshot is taken locally or with `--zfs-cmd`. 8 static builds (`build-all.ps1`, `CGO_ENABLED=0`):
  linux (amd64, arm64), darwin (amd64, arm64), windows, freebsd, illumos and solaris (amd64).
- Proxmox mode: runs **on the Proxmox host** (not inside a guest) - it shells out to
  `zfs`, `qm`/`pct` conventions apply, and talks to the QGA/QMP sockets
  that only exist on the host.
- Go 1.21+ to build.
- Linux-only for the LXC freezer (`//go:build linux`); the QEMU freezer
  itself has no OS constraint but is only meaningful where Proxmox/QEMU
  actually runs.
- No runtime dependencies beyond `golang.org/x/sys` (for the `FIFREEZE`/
  `FITHAW` ioctls, which aren't exported by that package and are defined
  directly in `freezer/lxc.go`).

## Test results

**ESXi (v1.1.0):** unit tests use an in-process fake ssh server (keyboard-interactive login, canned `vim-cmd`/`esxcli`
output taken from a real host) and a fake vSphere SOAP endpoint that, like the free license, refuses write calls
unless the User-Agent starts with "VMware". End-to-end against a real ESXi 8 (free license, NFS datastores of a ZFS
server): `discover`, `freeze`/`thaw`, `snap`, `cleanup` over ssh and soap, the quiesce/mem/plain modes on a powered-off
test VM, thaw after cleanup, wrong password and unreachable host (best-effort and `--forcefreeze`).
Also end-to-end from napp-it CS replication jobs (frontend on Windows, ZFS server on OmniOS, real ESXi 8; job setting
`freeze=esxi_soap` and `esxi_ssh`, host table with password): with two running VMs that have VMware Tools the run took
9-12 s for freeze, ZFS snapshot and thaw, the VM snapshots were removed again, and the incremental replication of the
NFS dataset finished ok. One VM (tn_scale) got a quiesced snapshot; on the other one (w2019) the quiesce failed inside
the guest and the tool fell back to a plain snapshot as documented (`strategy: "esxi-snap"`, warning in the result).
A running VM with a disk on another datastore was skipped as documented. Not tested: NFS 4.1 datastores,
ESXi 6.x/7.x, restoring a hotsnap ZFS snapshot (clone/rollback and revert of the VM snapshot).

**Proxmox:**

Beyond the unit test suite (`go test ./...`, including `-race`), this tool
was validated end-to-end against a real Proxmox 9 host (`pve`,
kernel 6.17.2-1-pve, Debian trixie/forky) with a mix of running/stopped
QEMU VMs and an LXC container, in addition to fully-scripted fake QGA/QMP
servers used for the automated test suite (`testhelpers/fake_qga`,
`testhelpers/fake_qmp`).

**Confirmed on this system: ZFS does not support `FIFREEZE`.**

```
$ fsfreeze -f /rpool/data/subvol-103-disk-0
fsfreeze: /rpool/data/subvol-103-disk-0: freeze failed: Operation not supported
```

This means the cgroup v2 fallback is the *primary* code path in practice
on this kernel/OpenZFS combination, not a rarely-exercised safety net.
Whether this holds on other kernel/OpenZFS version combinations is
untested - `FIFREEZE` support for ZFS has historically been inconsistent,
so don't assume either outcome without checking on your own system
(the command above is a 5-second way to check).

| Scenario tested | Result |
|---|---|
| LXC container (real ZFS, real cgroup v2) | `strategy: "cgroup"`, froze and thawed cleanly; snapshot verified present, then destroyed |
| Assumed cgroup path `/sys/fs/cgroup/lxc/<id>/cgroup.freeze` | Confirmed correct for this Proxmox version |
| VM with no `qemu-guest-agent` configured, real QMP socket | `strategy: "qmp-pause"` - froze and resumed via real `stop`/`cont`, VM unaffected afterward (`qm status` → `running`) |
| Neither QGA nor QMP reachable (fake sockets, both absent) | `strategy: "none"`, `status: "ok"` - snapshot still taken, exit code 0 |
| Same scenario with `--forcefreeze` | `status: "error"`, exit code 1, **no snapshot taken** - confirms the opt-in strict mode works as the inverse of the default |
| Guest with disks on two pools (`rpool/data` + `nvme480`) | `warnings` correctly flagged the disk outside the targeted dataset |
| Recursive snap + napp-it CS `include=/etc/pve` combo | `_include/_etc_pve` synced before the freeze; resulting snapshot confirmed (via `.zfs/snapshot/.../`) to contain the VM/CT `.conf` files alongside the frozen guest disk data, all in one atomic recursive snapshot |
| Snapshot cleanup after every test | Verified via `zfs list -t snapshot` before and after `zfs destroy -r` |

All source files transferred to the test host were verified byte-identical
(`sha256sum`) against the development copy before building, after an
earlier transfer method (a single very long base64-encoded blob) was found
to silently corrupt content in transit - plain source text over the same
channel transferred reliably. Builds and `go vet` were run natively on the
target architecture (linux/amd64) as well as cross-compiled for
linux/arm64.

## License

BSD 2-Clause - see [LICENSE](LICENSE). Same license as
[cs-sync](https://github.com/guenther-alka/cs-sync) and
[cs-stream](https://github.com/guenther-alka/cs-stream).
