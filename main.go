// cs-freeze4snap consistently snapshots a ZFS dataset backing Proxmox
// VM/LXC guests: it discovers which guests live on the dataset, freezes
// their I/O in parallel (best-effort), takes the ZFS snapshot, and thaws
// them again - guaranteed, even if the snapshot itself fails.
//
// Usage:
//
//	cs-freeze4snap snap --dataset rpool/data --name 20260809_1530
//	cs-freeze4snap snap --dataset rpool/data --name _repli_target_nr_123 \
//	    --exclude 1240,1241
//
// See README.md for the full flag reference and the platform support
// matrix (Proxmox QEMU/LXC today; Hyper-V and bhyve are designed for but
// not yet implemented - see freezer/freezer.go).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"cs-freeze4snap/freezer"
)

const version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "snap":
		os.Exit(runSnap(os.Args[2:]))
	case "version", "-version", "--version":
		fmt.Println("cs-freeze4snap", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `cs-freeze4snap - consistent ZFS snapshots for Proxmox VM/LXC guests

Usage:
  cs-freeze4snap snap --dataset <ds> --name <snapname> [options]

Options for 'snap':
  --dataset string        ZFS dataset to snapshot (required), e.g. rpool/data
  --name string            Snapshot name (required), e.g. 20260809_1530
  --recursive               Pass -r to zfs snapshot (default: true)
  --exclude string          Comma-separated VMIDs to skip freezing
  --include-only string     Comma-separated VMIDs; overrides discovery entirely
  --timeout duration        Max time to wait per guest freeze (default 30s)

Freeze is always best-effort by default: if a guest can't be frozen for
any reason (no guest agent, Proxmox/QMP not responding, unsupported
platform, ...), the snapshot is still taken "as-is" (crash-consistent for
that guest) rather than aborting the job. Check the "warnings" field in
the JSON result to see which guests, if any, were not cleanly frozen.

  --forcefreeze             Abort WITHOUT snapshotting if any guest could
                              not be cleanly frozen (default: false - never
                              aborts, snapshots as-is instead)`)
}

func runSnap(args []string) int {
	fs := flag.NewFlagSet("snap", flag.ExitOnError)
	dataset := fs.String("dataset", "", "ZFS dataset to snapshot (required)")
	name := fs.String("name", "", "snapshot name (required)")
	recursive := fs.Bool("recursive", true, "pass -r to zfs snapshot")
	excludeStr := fs.String("exclude", "", "comma-separated VMIDs to skip")
	includeStr := fs.String("include-only", "", "comma-separated VMIDs, overrides discovery")
	timeout := fs.Duration("timeout", 30*time.Second, "max time to wait per guest freeze")
	forceFreeze := fs.Bool("forcefreeze", false, "abort without snapshotting if any guest could not be cleanly frozen")
	fs.Parse(args)

	if *dataset == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "error: --dataset and --name are required")
		usage()
		return 2
	}

	result := runResult{Dataset: *dataset, Snapshot: *name}

	exclude, err := freezer.ParseIDList(*excludeStr)
	if err != nil {
		return result.fail(err)
	}
	includeOnly, err := freezer.ParseIDList(*includeStr)
	if err != nil {
		return result.fail(err)
	}

	guests, err := freezer.DiscoverProxmoxGuests(*dataset)
	if err != nil {
		return result.fail(fmt.Errorf("discovery: %w", err))
	}
	guests = freezer.FilterGuests(guests, includeOnly, exclude)

	if len(guests) == 0 {
		// Not necessarily an error - a dataset with no guests on it is a
		// valid (if unusual) target; snapshot proceeds without any freeze.
		result.Note = "no VM/LXC guests discovered under this dataset - snapshotting without freeze"
	}

	// Advisory check: does any guest have disks on a different pool that
	// -r cannot reach? Never fatal - if the check itself fails (e.g.
	// permissions), we log that and proceed with the snapshot regardless.
	if warnings, werr := freezer.DetectCrossPoolDisks(guests, *dataset); werr != nil {
		fmt.Fprintf(os.Stderr, "warning: cross-pool disk check failed (non-fatal): %v\n", werr)
	} else {
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "WARNING:", w)
		}
		result.Warnings = warnings
	}

	start := time.Now()
	sess := freezer.FreezeAll(guests, *timeout)
	defer sess.Thaw() // ALWAYS runs on any return path below, since fail()
	// and every other exit here is a plain `return`, not os.Exit(). main()
	// only calls os.Exit() once runSnap (and its defers) have fully returned.
	result.FreezeMs = time.Since(start).Milliseconds()

	if *forceFreeze {
		var unfrozen []int
		for _, r := range sess.Results() {
			if !r.Frozen {
				unfrozen = append(unfrozen, r.VMID)
			}
		}
		if len(unfrozen) > 0 {
			result.Guests = sess.Results()
			return result.fail(fmt.Errorf("--forcefreeze: %d guest(s) could not be cleanly frozen (vmids: %v) - aborting without snapshotting", len(unfrozen), unfrozen))
		}
	}

	// Default path (and --forcefreeze once every guest froze cleanly):
	// freeze is best-effort and never blocks the snapshot on its own.
	snapErr := freezer.Snapshot(*dataset, *name, *recursive)

	// Thaw explicitly here, before building the result, so Guests[].Thawed
	// reflects reality in the JSON we're about to emit. The deferred
	// sess.Thaw() above is a safety net for exit paths that reach return
	// without going through here (e.g. a future refactor) - Thaw() is
	// idempotent, so calling it again there is a harmless no-op.
	sess.Thaw()
	result.Guests = sess.Results()

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

type runResult struct {
	Dataset  string                `json:"dataset"`
	Snapshot string                `json:"snapshot"`
	Status   string                `json:"status"`
	Error    string                `json:"error,omitempty"`
	Note     string                `json:"note,omitempty"`
	Warnings []string              `json:"warnings,omitempty"`
	FreezeMs int64                 `json:"freeze_ms"`
	Guests   []freezer.GuestResult `json:"guests,omitempty"`
}

func emit(r runResult) {
	b, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "json marshal error:", err)
		fmt.Println(`{"status":"error","error":"internal: failed to marshal result"}`)
		return
	}
	fmt.Println(string(b))
}

// fail marks r as failed with err, preserving whatever fields (Dataset,
// Snapshot, Guests, ...) were already populated on r rather than discarding
// them - callers should always fail through an already-partially-filled
// result, never construct a fresh empty one.
func (r runResult) fail(err error) int {
	r.Status = "error"
	r.Error = err.Error()
	emit(r)
	return 1
}
