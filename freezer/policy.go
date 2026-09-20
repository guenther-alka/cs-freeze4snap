package freezer

// Freeze policy: which freeze steps are tried per guest, in which order, and
// how long each may take. Written as a "chain" in the cfg file (or with
// --policy flags):
//
//	[freeze,memory,zfs,30]               global default for every guest
//	192.168.2.48:vm100,memory,zfs        one VM on one server
//	192.168.2.48:*,freeze,plain          every VM on one server
//
// Steps are tried from left to right, the first that works wins:
//
//	freeze    ESXi: VM snapshot with the guest filesystem quiesced by VMware Tools
//	          Proxmox: QEMU guest agent fsfreeze / LXC fsfreeze (alias: quiesce)
//	memory    ESXi: VM snapshot with the RAM state (hot snapshot, slow)   (alias: mem)
//	          Proxmox VM: qm snapshot with the RAM state; it is NOT removed after the
//	          ZFS snapshot but kept as long as that ZFS snapshot exists (see qm.go)
//	plain     ESXi: VM snapshot of the disks only (crash-consistent)
//	pause     Proxmox QEMU: QMP stop/cont (the whole VM is paused)
//	zfs       give up on VM/guest freezing, take the ZFS snapshot as it is
//
// A number is a timeout in seconds; "step:60" applies to that step only, a bare
// number to every step without its own. A chain that does not end with zfs is
// strict: if no step works the run aborts before the ZFS snapshot (like
// --forcefreeze, but per guest).

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Step kinds of a chain.
const (
	StepQuiesce = "freeze"
	StepMemory  = "memory"
	StepPlain   = "plain"
	StepPause   = "pause"
	StepZFS     = "zfs"
)

// Step is one entry of a chain.
type Step struct {
	Kind    string
	Timeout time.Duration // 0: the chain's, else the tool's default
}

// Chain is an ordered list of freeze steps.
type Chain struct {
	Steps   []Step // without the terminating zfs
	ZFS     bool   // ends with zfs: best effort - the ZFS snapshot is taken whatever happens
	Timeout time.Duration
}

// Strict reports that the guest must be frozen, or the run aborts.
func (c Chain) Strict() bool { return !c.ZFS }

// String is the canonical text form, e.g. "freeze,memory:120,zfs,30".
func (c Chain) String() string {
	var p []string
	for _, s := range c.Steps {
		if s.Timeout > 0 {
			p = append(p, fmt.Sprintf("%s:%d", s.Kind, int(s.Timeout/time.Second)))
		} else {
			p = append(p, s.Kind)
		}
	}
	if c.ZFS {
		p = append(p, StepZFS)
	}
	if c.Timeout > 0 {
		p = append(p, strconv.Itoa(int(c.Timeout/time.Second)))
	}
	return strings.Join(p, ",")
}

// StepTimeout is the time step s may take: its own, the chain's, else def.
func (c Chain) StepTimeout(s Step, def time.Duration) time.Duration {
	switch {
	case s.Timeout > 0:
		return s.Timeout
	case c.Timeout > 0:
		return c.Timeout
	}
	return def
}

// StepsFor returns the steps that exist on platform p and the names of those
// that do not (memory on Proxmox, pause on ESXi, ...).
func (c Chain) StepsFor(p Platform) (steps []Step, unsupported []string) {
	for _, s := range c.Steps {
		ok := false
		switch p {
		case PlatformESXi:
			ok = s.Kind == StepQuiesce || s.Kind == StepMemory || s.Kind == StepPlain
		case PlatformProxmoxQEMU:
			ok = s.Kind == StepQuiesce || s.Kind == StepPause || s.Kind == StepMemory
		case PlatformProxmoxLXC:
			ok = s.Kind == StepQuiesce
		}
		if ok {
			steps = append(steps, s)
		} else {
			unsupported = append(unsupported, s.Kind)
		}
	}
	return steps, unsupported
}

// Built-in chains, used when no policy line names the guest.
var (
	// freeze, else a memory snapshot (RAM state kept, safe to restore without any
	// guest tools), else the ZFS snapshot as it is. plain and pause only give
	// crash-consistent points and have to be asked for.
	DefaultESXiChain    = Chain{Steps: []Step{{Kind: StepQuiesce}, {Kind: StepMemory}}, ZFS: true}
	DefaultProxmoxChain = Chain{Steps: []Step{{Kind: StepQuiesce}, {Kind: StepMemory}}, ZFS: true}

	// modeFreezeChain is what "--mode freeze" (= quiesce, the v1.1.0 default) stands for.
	modeFreezeChain = Chain{Steps: []Step{{Kind: StepQuiesce}, {Kind: StepPlain}}, ZFS: true}
)

// ChainOfMode maps the --mode flag (freeze, mem, plain) to a chain.
func ChainOfMode(mode string) (Chain, bool) {
	switch mode {
	case ModeQuiesce, ModeFreeze:
		return modeFreezeChain, true
	case ModeMem:
		return Chain{Steps: []Step{{Kind: StepMemory}, {Kind: StepPlain}}, ZFS: true}, true
	case ModePlain:
		return Chain{Steps: []Step{{Kind: StepPlain}}, ZFS: true}, true
	}
	return Chain{}, false
}

// ParseChain parses "freeze,memory:120,zfs,30" (a surrounding [ ] is allowed).
func ParseChain(s string) (Chain, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	var c Chain
	seen := map[string]bool{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if d, ok := parseSeconds(tok); ok {
			if c.Timeout > 0 {
				return Chain{}, fmt.Errorf("chain %q: two default timeouts", s)
			}
			c.Timeout = d
			continue
		}
		name, val, hasT := strings.Cut(tok, ":")
		var to time.Duration
		if hasT {
			d, ok := parseSeconds(val)
			if !ok {
				return Chain{}, fmt.Errorf("chain %q: bad timeout in %q (seconds expected)", s, tok)
			}
			to = d
		}
		switch name {
		case "quiesce": // VMware wording, accepted since v1.2.0
			name = StepQuiesce
		case "mem":
			name = StepMemory
		}
		switch name {
		case StepQuiesce, StepMemory, StepPlain, StepPause, StepZFS:
		default:
			return Chain{}, fmt.Errorf("chain %q: unknown step %q (freeze, memory, plain, pause, zfs)", s, name)
		}
		if seen[name] {
			return Chain{}, fmt.Errorf("chain %q: step %s twice", s, name)
		}
		seen[name] = true
		if c.ZFS {
			return Chain{}, fmt.Errorf("chain %q: zfs has to be the last step", s)
		}
		if name == StepZFS {
			c.ZFS = true
			continue
		}
		c.Steps = append(c.Steps, Step{Kind: name, Timeout: to})
	}
	if len(c.Steps) == 0 && !c.ZFS {
		return Chain{}, fmt.Errorf("chain %q is empty", s)
	}
	return c, nil
}

// parseSeconds reads "30" or "30s" (whole seconds, 1..86400).
func parseSeconds(s string) (time.Duration, bool) {
	s = strings.TrimSuffix(s, "s")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 86400 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// Policy holds the chains of one server: per VM, per server (*) and global.
type Policy struct {
	global   *Chain
	star     *Chain
	perVM    map[int]Chain
	override *Chain // --mode: wins over everything

	// memkeep: at most N Proxmox memory snapshots per VM (0: as many as there
	// are ZFS snapshots that need them). mkServer (host:memkeep=N, --policy) wins over mkGlobal.
	mkGlobal, mkServer int
}

// For resolves the chain of one guest (per VM, then *, then global).
func (p *Policy) For(vmid int) (Chain, bool) {
	if p == nil {
		return Chain{}, false
	}
	if p.override != nil {
		return *p.override, true
	}
	if c, ok := p.perVM[vmid]; ok {
		return c, true
	}
	if p.star != nil {
		return *p.star, true
	}
	if p.global != nil {
		return *p.global, true
	}
	return Chain{}, false
}

// SetOverride makes c the chain of every guest (the --mode flag).
func (p *Policy) SetOverride(c Chain) { p.override = &c }

// Merge lets the entries of o (the --policy flags) win over those of p (the cfg).
func (p *Policy) Merge(o *Policy) *Policy {
	if p == nil {
		p = &Policy{perVM: map[int]Chain{}}
	}
	if o == nil {
		return p
	}
	if o.global != nil {
		p.global = o.global
	}
	if o.star != nil {
		p.star = o.star
	}
	for id, c := range o.perVM {
		p.perVM[id] = c
	}
	if o.override != nil {
		p.override = o.override
	}
	if o.mkGlobal > 0 {
		p.mkGlobal = o.mkGlobal
	}
	if o.mkServer > 0 {
		p.mkServer = o.mkServer
	}
	return p
}

// MemKeep is the cap of Proxmox memory snapshots per VM, 0 = no cap.
func (p *Policy) MemKeep() int {
	if p == nil {
		return 0
	}
	if p.mkServer > 0 {
		return p.mkServer
	}
	return p.mkGlobal
}

// Empty reports that no chain is defined at all.
func (p *Policy) Empty() bool {
	return p == nil || (p.override == nil && p.global == nil && p.star == nil && len(p.perVM) == 0)
}

// parseVMSpec reads vm100, ct100, 100 or *.
func parseVMSpec(s string) (id int, star bool, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "*" {
		return 0, true, true
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "vm"), "ct")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false, false
	}
	return n, false, true
}

// stripComment removes a trailing " # comment" of a policy line.
func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "\t#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// IsPolicyLine reports whether a cfg line belongs to the policy, so the
// connection parsers can skip it: a [chain] line, a "host:..." line, or a
// chain= / vm100= / ct100= line of the key=value form.
func IsPolicyLine(t string) bool {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "[") {
		return true
	}
	first := t
	if i := strings.IndexAny(t, ",="); i >= 0 {
		first = t[:i]
	}
	if strings.Contains(first, ":") {
		return true // 192.168.2.48:vm100,... or 192.168.2.48:proto=ssh
	}
	if i := strings.Index(t, "="); i > 0 {
		k := strings.ToLower(strings.TrimSpace(t[:i]))
		if k == "chain" || k == "memkeep" {
			return true
		}
		if _, star, ok := parseVMSpec(k); ok && !star && (strings.HasPrefix(k, "vm") || strings.HasPrefix(k, "ct")) {
			return true
		}
	}
	return false
}

// ParsePolicy reads the policy lines of a cfg text. With host != "" only
// "host:..." lines of that host count (plus the global [chain] and chain=
// lines); with host == "" the lines carry no host prefix (--policy flags).
func ParsePolicy(name, text, host string) (*Policy, error) {
	return parsePolicy(name, text, host, false)
}

// parsePolicy: with flags every non-blank line is a policy line (--policy).
func parsePolicy(name, text, host string, flags bool) (*Policy, error) {
	p := &Policy{perVM: map[int]Chain{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	n := 0
	for sc.Scan() {
		n++
		t := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string([]byte{0xEF, 0xBB, 0xBF})))
		if t == "" || strings.HasPrefix(t, "#") || (!flags && !IsPolicyLine(t)) {
			continue
		}
		bad := func(err error) (*Policy, error) { return nil, fmt.Errorf("%s line %d: %w", name, n, err) }
		t = stripComment(t)
		switch {
		case strings.HasPrefix(t, "["):
			c, err := ParseChain(t)
			if err != nil {
				return bad(err)
			}
			p.global = &c
		case strings.HasPrefix(strings.ToLower(t), "chain="):
			c, err := ParseChain(t[len("chain="):])
			if err != nil {
				return bad(err)
			}
			p.global = &c
		default:
			var h, rest string
			if i := strings.IndexAny(t, ",="); i >= 0 && strings.Contains(t[:i], ":") {
				h, rest, _ = strings.Cut(t, ":")
			} else {
				rest = t // no host prefix: --policy "vm100,memory" or vm100=memory
			}
			if h != "" && !strings.EqualFold(h, host) && host != "" {
				continue // another server
			}
			if h != "" && host == "" {
				continue // host lines are not for --policy flags
			}
			if k, v, ok := strings.Cut(rest, "="); ok && strings.EqualFold(strings.TrimSpace(k), "memkeep") {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 0 || n > 999 {
					return bad(fmt.Errorf("bad memkeep %q (0..999, 0 = no cap)", strings.TrimSpace(v)))
				}
				if h != "" || flags {
					p.mkServer = n
				} else {
					p.mkGlobal = n
				}
				continue
			}
			spec, chain, ok := strings.Cut(rest, ",")
			if !ok {
				spec, chain, ok = strings.Cut(rest, "=")
			}
			if !ok {
				return bad(fmt.Errorf("expected vm100,<chain>"))
			}
			if _, isOpt := optionKeys[strings.ToLower(strings.TrimSpace(spec))]; isOpt {
				continue // host:proto=ssh - a connection option, not a chain
			}
			id, star, ok := parseVMSpec(spec)
			if !ok {
				return bad(fmt.Errorf("%q is not a VM (vm100, ct100, 100 or *)", strings.TrimSpace(spec)))
			}
			c, err := ParseChain(chain)
			if err != nil {
				return bad(err)
			}
			if star {
				p.star = &c
			} else {
				p.perVM[id] = c
			}
		}
	}
	return p, sc.Err()
}

// optionKeys are the connection options that may follow "host:" in a cfg.
var optionKeys = map[string]bool{"proto": true, "hostkey": true, "tls_sha256": true, "port": true, "timeout": true, "useragent": true}

// LoadPolicy reads the policy of a cfg file for one server ("" = no file).
func LoadPolicy(path, host string) (*Policy, error) {
	if path == "" {
		return &Policy{perVM: map[int]Chain{}}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cfg: %w", err)
	}
	return ParsePolicy("cfg "+path, string(b), host)
}

// PolicyOfFlags builds a policy from --policy values ("[chain]", "vm100,chain").
func PolicyOfFlags(lines []string) (*Policy, error) {
	return parsePolicy("--policy", strings.Join(lines, "\n"), "", true)
}

var currentPolicy *Policy

// SetPolicy installs the policy the freezers consult (nil: built-in chains only).
func SetPolicy(p *Policy) { currentPolicy = p }

// MemKeep is the cap of Proxmox memory snapshots per VM from the policy (0: none).
func MemKeep() int { return currentPolicy.MemKeep() }

// ChainFor is the chain of guest g: the policy's, else def.
func ChainFor(g Guest, def Chain) Chain {
	if c, ok := currentPolicy.For(g.VMID); ok {
		return c
	}
	return def
}

// DefaultLXCChain: containers know no pause.
var DefaultLXCChain = Chain{Steps: []Step{{Kind: StepQuiesce}}, ZFS: true}

// DefaultChainFor is the built-in chain of a platform.
func DefaultChainFor(p Platform, t GuestType) Chain {
	switch {
	case p == PlatformESXi:
		return DefaultESXiChain
	case p == PlatformProxmoxLXC || t == TypeLXC:
		return DefaultLXCChain
	}
	return DefaultProxmoxChain
}

// StrictViolations lists the guests that were not frozen although their chain
// has no zfs fallback: the run has to abort before the ZFS snapshot.
func StrictViolations(rs []GuestResult) []int {
	var ids []int
	for _, r := range rs {
		if !r.Frozen && r.Strict && r.Strategy != StrategyZFSOnly {
			ids = append(ids, r.VMID)
		}
	}
	return ids
}
