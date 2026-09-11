# cs-freeze4snap

Consistent ZFS snapshots for Proxmox VM/LXC guests - freeze if possible,
snapshot regardless.

Part of the [napp-it CS](https://napp-it.org) cluster tooling family
(alongside [cs-sync](https://github.com/guenther-alka/cs-sync) and
[cs-stream](https://github.com/guenther-alka/cs-stream)).
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
      "strategy": "qmp-pause", // "qga" | "qmp-pause" | "fsfreeze" | "cgroup" | "none"
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

- Runs **on the Proxmox host** (not inside a guest) - it shells out to
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
