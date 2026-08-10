# Changelog

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
