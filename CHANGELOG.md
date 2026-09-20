# Changelog

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
