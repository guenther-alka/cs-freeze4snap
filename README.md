# cs-freeze4snap

Consistent ZFS snapshots for Proxmox VM/LXC guests - freeze if possible,
snapshot regardless.

Part of the [napp-it CS](https://napp-it.org) cluster tooling family
(alongside [cs-sync](https://github.com/guenther-alka/cs-sync) and
[cs-stream](https://github.com/guenther-alka/cs-stream)).

## Goal

`zfs snapshot` on its own is only *crash-consistent*: it captures whatever
happens to be on disk at that instant, the storage-level equivalent of
pulling the power cord. For a VM or container that's actively writing, that
can mean a snapshot mid-write - usually fine for journaled filesystems, not
guaranteed for anything else.

`cs-freeze4snap` sits in front of `zfs snapshot` and tries to get a better
consistency guarantee for every Proxmox VM/LXC guest living on the target
dataset, **without ever blocking the snapshot from happening**. The guiding
principle, in the project owner's words:

> mache zfs snapshot mit freeze vm sofern möglich und das so gut wie es
> eben geht - take the ZFS snapshot with a VM freeze if at all possible,
> and do it as well as it can be done.

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
