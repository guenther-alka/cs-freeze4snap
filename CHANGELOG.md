# Changelog

## v1.3.0 - Proxmox memory snapshots, `freeze` as step name, new default chain

- New: the **`memory` step for Proxmox VMs**: `qm snapshot <vmid> cs4s_<snapshot> --vmstate 1` keeps the RAM state,
  so a restore point is safe without a guest agent or any guest tools (like the ESXi hot snapshot). Unlike ESXi
  the VM snapshot is **not** removed after the ZFS snapshot: it is the consistent restore point of that ZFS
  snapshot and lives as long as the ZFS snapshot exists (every run removes the `cs4s_` snapshots of a VM whose
  ZFS snapshot is gone, 10 minutes grace for parallel jobs). Only snapshots named `cs4s_*` with the description
  `cs-freeze4snap ...` are ever touched. Strategy `qm-mem`.
- New: `memkeep=N` (global line, `host:memkeep=N`, or `--policy 'memkeep=N'`): optional cap of memory snapshots
  per VM (newest N are kept). Default 0 = no cap. Proxmox reserves the vmstate volume thick (about 2.2 x RAM).
- New: `--pre-snap-cmd 'cmd'`: shell command run after the freeze and right before `zfs snapshot`, 120 s, failure =
  warning. napp-it CS uses it to sync `/etc/pve` into `<dataset>/_include/_etc_pve` after the memory snapshot was
  taken, so the copied VM config contains the snapshot section that `qm rollback` needs.
- Restore: `zfs rollback -r <pool>/vm-N-disk-M@cs4s_<snapshot>` for every disk, then `qm rollback N cs4s_<snapshot>`
  (verified: the VM starts with `-loadstate`). The first step is needed because Proxmox refuses `qm rollback`
  while a newer ZFS snapshot exists on the disk. See README "Proxmox VMs: the memory step".
- New: a Proxmox VM that is **not running** counts as consistent (strategy `stopped`, no snapshot) and does not
  break a strict chain.
- Changed: the step is now called **`freeze`** (that is what Proxmox/QEMU call it); `quiesce` (VMware wording, used
  by v1.2.0) is still accepted as an alias, also for `--mode` (`--mode freeze|mem|plain`). Chains are written back as
  `freeze`; the strategy `esxi-quiesce` in the JSON is unchanged.
- Changed: **new built-in default chain `freeze,memory,zfs`** for ESXi and Proxmox VMs (before: ESXi
  `quiesce,plain,zfs`, Proxmox VM `quiesce,pause,zfs`): a restore point is filesystem-consistent with working guest
  tools and running-state-consistent without them; `plain`/`pause` (crash-consistent) have to be asked for. LXC stays
  `freeze,zfs`. `--mode freeze` keeps meaning `freeze,plain,zfs`. To get the old behaviour set
  `[freeze,plain,zfs]` / `[freeze,pause,zfs]` in the cfg or `--policy`.
- The Proxmox snapshot timeout of a memory step defaults to 120 s (writing the RAM takes longer than a QGA freeze).
- Note: `TestSOAPLoginAndPin` fails on Windows (as in v1.1.0/v1.2.0, a test-environment issue); all tests pass on Linux.

## v1.2.0 - freeze chains with timeouts, proto auto

- New: **freeze chains** per guest, e.g. `[quiesce,memory,zfs,30]`: steps `quiesce`, `memory`, `plain`,
  `pause`, `zfs` are tried from left to right, the first that works wins. `memory` (ESXi) takes a hot
  snapshot including the RAM state when quiesce is not possible (no VMware Tools). Each step has its own
  timeout (`memory:300`, or a bare number for the whole chain); a step that times out counts as failed and
  the next one starts (soap: the vSphere task is cancelled; a snapshot the host may still have created is
  removed again).
- Chains are set in the cfg file - global `[...]`, per server `host:*,...`, per VM `host:vm100,...` - or with
  `--policy` (repeatable). Precedence: `--mode`, `--policy`, VM, `*`, global, built-in default.
  Steps that do not exist on a platform are skipped.
- A chain **without** `zfs` is strict: if no step works the run aborts before the ZFS snapshot (per guest,
  like `--forcefreeze`). A chain of only `zfs` takes no VM snapshot for that guest (`zfs-only`).
- `guests[].chain` / `strict` in the JSON result.
- `--proto auto` is the new default for ESXi: soap first, ssh if soap cannot be reached (warning); a failed soap
  login is not retried over ssh (account lockout). The cfg host table takes per-host option lines
  `host:proto=`, `host:port=`, `host:hostkey=`, `host:tls_sha256=`, `host:timeout=`, `host:useragent=`.
- `--mode` now defaults to "" (the built-in chain) and acts as a shortcut chain for all VMs.
- The key=value cfg form accepts `chain=` and `vm100=` lines.
- Unchanged: Proxmox behaviour without `--policy`/cfg (default chain `quiesce,pause,zfs`) and the JSON of
  v1.1.0 apart from the new fields.

## v1.1.0 - ESXi hotsnap (ssh or soap), remote use

- New: hotsnap of all VMs on an NFS datastore of an ESXi host. `snap --hypervisor esxi`
  (VM snapshots -> ZFS snapshot -> remove VM snapshots) and the separate steps
  `discover`, `freeze`, `thaw`, `cleanup`. See README "ESXi".
- The VMs are found by the tool: `--storage nfs` selects the NFS datastores that mount the
  dataset's export (`--nfs-path`, `--nfs-server`, child datasets with `--recursive`), `--vms
  all|id,name,...` (alias `--snap`) picks the VMs. Powered-off/suspended VMs and VMs with disks on
  other datastores are skipped and reported (`--include-off`, `--allow-mixed`).
- Two transports: `--proto ssh` (`vim-cmd`, keyboard-interactive password or key, host key pin) and
  `--proto soap` (vSphere API, sends a "VMware ..." User-Agent because the free ESXi license rejects
  write calls from other clients; certificate pin). Connection data in `--cfg`: a host table
  (`host,user,password` / `host,cert[,keyfile]`, one server per line, selected with `--host`) or a
  key=value file with the extra options; `CS_ESXI_*` environment; never the password as a flag.
- The tool may run on any machine that reaches the ESXi host; `--zfs-cmd` takes the ZFS snapshot on
  another machine (`{dataset}`, `{snapshot}`, `{fullname}`).
- Modes `quiesce` (default), `mem`, `plain`; quiesce/mem fall back to plain, reported as a warning.
  Best-effort as before: an unreachable host or failed VM snapshot never blocks the ZFS snapshot
  (`--forcefreeze` aborts). `thaw` is idempotent; `cleanup` removes only own tagged snapshots.
- `--timeout` defaults to 120 s for ESXi. `CS_ESXI_DEBUG=1` traces calls with timings.
- 8 builds (`build-all.ps1`): linux (amd64, arm64), darwin (amd64, arm64), windows, freebsd, illumos, solaris (amd64). Proxmox behaviour is unchanged: snap without --hypervisor gives byte-identical JSON, exit codes and zfs calls as v1.0.0 (checked on 10 scenarios incl. --exclude, --include-only, --forcefreeze, snapshot failure, bad arguments).
- Verified against a real ESXi 8 (free license), ssh and soap: discover, freeze/thaw, snap, cleanup,
  wrong password, unreachable host, and from napp-it CS replication jobs (esxi_soap and esxi_ssh, two running VMs
  with VMware Tools: one quiesced snapshot, one fallback to plain after a guest-side quiesce failure). Not tested:
  NFS 4.1 datastores, ESXi 6.x/7.x, restoring a hotsnap ZFS snapshot.
- napp-it CS: job setting "Freeze VM via freeze4snap" = off | esxi_ssh | esxi_soap | proxmox_local plus VM server IP
  (see README "Use from napp-it CS jobs").

## v1.0.0 - first stable release

- Promoted from v0.1.0 to v1.0.0: functionality unchanged, marks the
  release as verified/stable for general use (see v0.1.0 notes below
  for full feature set).

## v0.1.0 - initial release

- ZFS dataset snapshot with automatic Proxmox VM/LXC guest discovery
  (matches `vm-<id>-disk-*` / `subvol-<id>-disk-*` naming convention)
- VM freeze cascade: QEMU Guest Agent fsfreeze → QMP stop/cont → snapshot
  as-is (never blocks the job)
- LXC freeze: host-side FIFREEZE ioctl → cgroup v2 freeze fallback
- Best-effort by default; `--forcefreeze` for strict abort-on-incomplete-freeze
- Cross-pool disk detection (`zfs snapshot -r` doesn't cross pool boundaries)
- `--exclude` / `--include-only` VMID overrides for discovery
- Pluggable `Freezer` interface/registry, ready for additional hypervisor
  backends (Hyper-V, bhyve - not yet implemented)
- Verified end-to-end against a real Proxmox 9 host (VM via QMP fallback,
  LXC via cgroup fallback - `FIFREEZE` unsupported on this ZFS/kernel
  combination, see README)
