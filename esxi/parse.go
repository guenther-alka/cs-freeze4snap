package esxi

import (
	"regexp"
	"strconv"
	"strings"
)

// Parsers for the text output of vim-cmd and esxcli. They live apart from the
// SSH code so they can be tested against captured output.

var (
	reVMLine   = regexp.MustCompile(`^(\d+)\s+(.*?)\s+\[([^\]]+)\]\s+(\S.*?\.vmx)`)
	reSnapName = regexp.MustCompile(`Snapshot Name\s*:\s?(.*)$`)
	reSnapID   = regexp.MustCompile(`Snapshot Id\s*:\s*(\d+)`)
	reSnapDesc = regexp.MustCompile(`Snapshot Desc\w*\s*:\s?(.*)$`)
	reDSName   = regexp.MustCompile(`^name\s+(.*?)\s*$`)
	reColSplit = regexp.MustCompile(`\s{2,}`)
)

// parseGetAllVMs reads "vim-cmd vmsvc/getallvms": vmid, name and the datastore
// of the .vmx file. VMs whose line cannot be understood are skipped.
func parseGetAllVMs(out string) []VM {
	var vms []VM
	for _, l := range strings.Split(out, "\n") {
		m := reVMLine.FindStringSubmatch(strings.TrimRight(l, "\r"))
		if m == nil {
			continue
		}
		id, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		vms = append(vms, VM{ID: id, Name: m[2], Datastores: []string{m[3]}})
	}
	return vms
}

// parseNFSList reads "esxcli storage nfs list" / "nfs41 list". Columns are cut
// at the positions of the dashed header line so volume names with blanks work.
func parseNFSList(out, typ string) []Datastore {
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	var cols [][2]int
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "---") {
			start = i
			// column spans = runs of '-'
			in := false
			b := 0
			for j := 0; j <= len(l); j++ {
				dash := j < len(l) && l[j] == '-'
				if dash && !in {
					b, in = j, true
				}
				if !dash && in {
					cols = append(cols, [2]int{b, j})
					in = false
				}
			}
			break
		}
	}
	if start < 0 || len(cols) < 3 {
		return nil
	}
	field := func(l string, c int) string {
		s, e := cols[c][0], cols[c][1]
		if c == len(cols)-1 || e > len(l) {
			if s >= len(l) {
				return ""
			}
			if c == len(cols)-1 {
				return strings.TrimSpace(l[s:])
			}
			e = len(l)
		}
		return strings.TrimSpace(l[s:e])
	}
	var ds []Datastore
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		host := field(l, 1)
		if i := strings.IndexAny(host, ", "); i > 0 { // nfs41 may list several servers
			host = host[:i]
		}
		ds = append(ds, Datastore{Name: field(l, 0), Type: typ, Host: host, Path: field(l, 2)})
	}
	return ds
}

// parsePowerState turns "Powered on" / "Powered off" / "Suspended" into the
// SOAP names. Unknown text yields "".
func parsePowerState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(s, "powered on"):
		return PowerOn
	case strings.HasSuffix(s, "powered off"):
		return PowerOff
	case strings.HasSuffix(s, "suspended"):
		return PowerSuspended
	}
	return ""
}

// parseSnapshotGet reads "vim-cmd vmsvc/snapshot.get <id>" (a tree printed
// depth-first with ROOT/CHILD markers - only the flat list is needed).
func parseSnapshotGet(out string) []Snapshot {
	var snaps []Snapshot
	var cur *Snapshot
	flush := func() {
		if cur != nil && cur.ID != "" {
			snaps = append(snaps, *cur)
		}
		cur = nil
	}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if m := reSnapName.FindStringSubmatch(l); m != nil {
			flush()
			cur = &Snapshot{Name: strings.TrimSpace(m[1])}
			continue
		}
		if cur == nil {
			continue
		}
		if m := reSnapID.FindStringSubmatch(l); m != nil {
			cur.ID = m[1]
		} else if m := reSnapDesc.FindStringSubmatch(l); m != nil {
			cur.Desc = strings.TrimSpace(m[1])
		}
	}
	flush()
	return snaps
}

// parseLoop reads the output of the per-VM query script used by the SSH
// transport: blocks "@@<vmid>", a power state line, then "name <datastore>"
// lines.
func parseLoop(out string) map[int]*VM {
	res := map[int]*VM{}
	var cur *VM
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			id, err := strconv.Atoi(strings.TrimPrefix(l, "@@"))
			if err != nil {
				cur = nil
				continue
			}
			cur = &VM{ID: id}
			res[id] = cur
			continue
		}
		if cur == nil {
			continue
		}
		if p := parsePowerState(l); p != "" && cur.PowerState == "" {
			cur.PowerState = p
			continue
		}
		if m := reDSName.FindStringSubmatch(l); m != nil {
			// "vim-cmd vmsvc/get.datastores" prints ONE line "name  <ds1>  <ds2> ..."
			// with the datastores in padded columns - split on 2+ blanks so that
			// a name holding a single blank survives.
			for _, n := range reColSplit.Split(m[1], -1) {
				if n = strings.TrimSpace(n); n != "" {
					cur.Datastores = append(cur.Datastores, n)
				}
			}
		}
	}
	return res
}
