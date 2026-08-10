package freezer

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Proxmox's own storage plugin names zvols/datasets predictably:
//
//	vm-<vmid>-disk-<n>      -> QEMU/KVM VM disk (zvol)
//	subvol-<vmid>-disk-<n>  -> LXC container rootfs/mp (dataset)
//
// This lets us infer guest type purely from ZFS naming, without needing
// `qm config` / `pct config` lookups for the discovery pass itself.
var (
	reVMDisk  = regexp.MustCompile(`(?:^|/)vm-(\d+)-disk-\d+$`)
	reLXCDisk = regexp.MustCompile(`(?:^|/)subvol-(\d+)-disk-\d+$`)
)

// DiscoverProxmoxGuests lists all VM/LXC guests whose backing storage lives
// under the given ZFS dataset (recursively), by parsing `zfs list`. Multiple
// disks belonging to the same VMID are deduplicated - one Guest entry per
// VMID, since freeze/thaw operates at the guest level, not per-disk.
func DiscoverProxmoxGuests(dataset string) ([]Guest, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name", "-r", dataset).Output()
	if err != nil {
		return nil, fmt.Errorf("zfs list -r %s: %w", dataset, err)
	}
	return parseZfsListOutput(string(out)), nil
}

// parseZfsListOutput contains the actual Proxmox-naming-convention parsing
// logic, kept separate from the exec.Command call above so it can be unit
// tested with canned `zfs list` output instead of a real ZFS pool.
func parseZfsListOutput(out string) []Guest {
	seen := map[int]Guest{}
	// preserve first-seen order for deterministic, readable output
	var order []int

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if m := reVMDisk.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			if _, ok := seen[id]; !ok {
				order = append(order, id)
			}
			seen[id] = Guest{VMID: id, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: line}
			continue
		}
		if m := reLXCDisk.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			if _, ok := seen[id]; !ok {
				order = append(order, id)
			}
			seen[id] = Guest{VMID: id, Type: TypeLXC, Platform: PlatformProxmoxLXC, Dataset: line}
			continue
		}
		// anything else under the dataset (plain datasets, unrelated zvols,
		// nested pool structure) is silently ignored - not every leaf under
		// a storage dataset is a guest disk.
	}

	guests := make([]Guest, 0, len(order))
	for _, id := range order {
		guests = append(guests, seen[id])
	}
	return guests
}

// allDiskDatasetsByVMID scans zfs list output (any scope - typically an
// unrestricted, pool-wide `zfs list`) and returns every matching
// vm-*/subvol-* dataset path grouped by VMID, without deduplication. Used
// by DetectCrossPoolDisks to find disks belonging to an already-discovered
// guest that live outside the target dataset (e.g. on a different pool).
func allDiskDatasetsByVMID(out string) map[int][]string {
	result := map[int][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if m := reVMDisk.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			result[id] = append(result[id], line)
			continue
		}
		if m := reLXCDisk.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			result[id] = append(result[id], line)
			continue
		}
	}
	return result
}

// DetectCrossPoolDisks checks whether any of the given (already discovered)
// guests have additional disks living outside targetDataset - most notably
// on a different pool entirely, which `zfs snapshot -r targetDataset@name`
// cannot reach regardless of the -r flag, since recursion never crosses
// pool boundaries. Returns one human-readable warning string per affected
// guest; an empty (nil) slice means no cross-pool disks were found.
//
// This is advisory only: a failure to run the global `zfs list` (e.g.
// permissions) returns an error but must NOT be treated as fatal by the
// caller - the snapshot itself can still proceed correctly, it just means
// this particular safety check couldn't be performed.
func DetectCrossPoolDisks(guests []Guest, targetDataset string) ([]string, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name").Output()
	if err != nil {
		return nil, fmt.Errorf("zfs list (pool-wide, for cross-pool check): %w", err)
	}
	allByVMID := allDiskDatasetsByVMID(string(out))

	prefix := targetDataset + "/"
	var warnings []string
	for _, g := range guests {
		var outside []string
		for _, d := range allByVMID[g.VMID] {
			if d != targetDataset && !strings.HasPrefix(d, prefix) {
				outside = append(outside, d)
			}
		}
		if len(outside) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"vmid %d (%s) has disk(s) outside %s that will NOT be included in this snapshot: %s",
				g.VMID, g.Type, targetDataset, strings.Join(outside, ", ")))
		}
	}
	return warnings, nil
}

// FilterGuests applies --include-only / --exclude overrides on top of
// discovery results. includeOnly, if non-empty, replaces discovery entirely
// (the caller is asserting these are the correct VMIDs) - discovery is still
// used to resolve VMID -> Guest{} details rather than trusting the caller's
// bare IDs blindly.
func FilterGuests(discovered []Guest, includeOnly, exclude []int) []Guest {
	if len(includeOnly) > 0 {
		want := toSet(includeOnly)
		var out []Guest
		for _, g := range discovered {
			if want[g.VMID] {
				out = append(out, g)
			}
		}
		return out
	}
	if len(exclude) > 0 {
		skip := toSet(exclude)
		var out []Guest
		for _, g := range discovered {
			if !skip[g.VMID] {
				out = append(out, g)
			}
		}
		return out
	}
	return discovered
}

func toSet(ids []int) map[int]bool {
	m := make(map[int]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// ParseIDList parses a comma-separated VMID list like "1234,1235" from a
// CLI flag. Empty string yields an empty (nil) slice.
func ParseIDList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var ids []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid VMID %q: %w", part, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
