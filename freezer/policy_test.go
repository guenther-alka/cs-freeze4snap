package freezer

import (
	"strings"
	"testing"
	"time"
)

func TestParseChain(t *testing.T) {
	c, err := ParseChain("[freeze,mem:120,zfs,30]")
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != "quiesce,memory:120,zfs,30" || !c.ZFS || c.Strict() || c.Timeout != 30*time.Second {
		t.Errorf("chain: %q %+v", c.String(), c)
	}
	if c, _ = ParseChain("memory,plain"); !c.Strict() {
		t.Error("a chain without zfs must be strict")
	}
	if c, err = ParseChain("zfs"); err != nil || len(c.Steps) != 0 || !c.ZFS {
		t.Errorf("zfs only: %+v %v", c, err)
	}
	for _, bad := range []string{"", "bogus", "zfs,memory", "memory,memory", "memory:x", "10,20,zfs", "quiesce:0"} {
		if _, err := ParseChain(bad); err == nil {
			t.Errorf("chain %q accepted", bad)
		}
	}
}

func TestStepTimeout(t *testing.T) {
	c, _ := ParseChain("quiesce,memory:300,zfs,30")
	if got := c.StepTimeout(c.Steps[0], 120*time.Second); got != 30*time.Second {
		t.Errorf("chain timeout: %v", got)
	}
	if got := c.StepTimeout(c.Steps[1], 120*time.Second); got != 300*time.Second {
		t.Errorf("step timeout: %v", got)
	}
	d, _ := ParseChain("quiesce,zfs")
	if got := d.StepTimeout(d.Steps[0], 120*time.Second); got != 120*time.Second {
		t.Errorf("default timeout: %v", got)
	}
}

const policyCfg = `# servers
192.168.2.48,root,secret
192.168.2.49,root,secret
[quiesce,memory,zfs,30]
192.168.2.48:*,quiesce,plain,zfs
192.168.2.48:vm100,memory,zfs        # a comment
192.168.2.48:proto=ssh
192.168.2.49:vm100,plain
`

func TestParsePolicyPrecedence(t *testing.T) {
	p, err := ParsePolicy("t", policyCfg, "192.168.2.48")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := p.For(100); c.String() != "memory,zfs" {
		t.Errorf("vm100: %s", c)
	}
	if c, _ := p.For(101); c.String() != "quiesce,plain,zfs" {
		t.Errorf("star: %s", c)
	}
	q, err := ParsePolicy("t", policyCfg, "192.168.2.49")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := q.For(100); c.String() != "plain" || !c.Strict() {
		t.Errorf("other host vm100: %s", c)
	}
	if c, _ := q.For(101); c.String() != "quiesce,memory,zfs,30" {
		t.Errorf("global: %s", c)
	}
	r, _ := ParsePolicy("t", policyCfg, "10.0.0.1")
	if c, _ := r.For(100); c.String() != "quiesce,memory,zfs,30" {
		t.Errorf("unlisted host gets the global chain: %s", c)
	}
	r.SetOverride(Chain{Steps: []Step{{Kind: StepPlain}}, ZFS: true})
	if c, _ := r.For(100); c.String() != "plain,zfs" {
		t.Errorf("override: %s", c)
	}
}

func TestParsePolicyErrorsAndFlags(t *testing.T) {
	if _, err := ParsePolicy("t", "192.168.2.48:vm100,bogus\n", "192.168.2.48"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("bad chain: %v", err)
	}
	if _, err := ParsePolicy("t", "192.168.2.48:xx,memory\n", "192.168.2.48"); err == nil {
		t.Error("bad vm accepted")
	}
	p, err := PolicyOfFlags([]string{"[quiesce,zfs,20]", "vm100,memory,zfs", "ct5,zfs"})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := p.For(100); c.String() != "memory,zfs" {
		t.Errorf("flag vm100: %s", c)
	}
	if c, _ := p.For(5); c.String() != "zfs" {
		t.Errorf("flag ct5: %s", c)
	}
	if c, _ := p.For(7); c.String() != "quiesce,zfs,20" {
		t.Errorf("flag global: %s", c)
	}
	// key=value form
	k, err := ParsePolicy("t", "host=1.2.3.4\npassword=a:b,c\nchain=memory,zfs\nvm7=quiesce\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := k.For(7); c.String() != "quiesce" {
		t.Errorf("vm7=: %s", c)
	}
	if c, _ := k.For(8); c.String() != "memory,zfs" {
		t.Errorf("chain=: %s", c)
	}
}

func TestIsPolicyLine(t *testing.T) {
	for l, want := range map[string]bool{
		"[memory,zfs]":                  true,
		"192.168.2.48:vm100,memory":     true,
		"192.168.2.48:proto=ssh":        true,
		"chain=memory,zfs":              true,
		"vm100=memory":                  true,
		"192.168.2.48,root,secret":      false,
		"192.168.2.48,root,pa:ss":       false,
		"password=a:b":                  false,
		"key=C:\\keys\\id_rsa":          false,
		"192.168.2.52,root,cert,/k/key": false,
	} {
		if got := IsPolicyLine(l); got != want {
			t.Errorf("IsPolicyLine(%q) = %v", l, got)
		}
	}
}

// --- chains at work on the fake ESXi host

func withPolicy(t *testing.T, text, host string) {
	t.Helper()
	p, err := ParsePolicy("t", text, host)
	if err != nil {
		t.Fatal(err)
	}
	SetPolicy(p)
	t.Cleanup(func() { SetPolicy(nil) })
}

func TestESXiChainMemoryFallback(t *testing.T) {
	withPolicy(t, "[quiesce,memory,zfs]\n", "h")
	tr := newFakeTr()
	tr.quiesceF[10] = true
	f, _ := NewESXiFreezer(tr, "", "s")
	Register(f)
	defer unregisterLast()

	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	defer sess.Thaw()
	for _, r := range sess.Results() {
		switch r.VMID {
		case 10:
			if !r.Frozen || r.Strategy != StrategyESXiMem || !strings.Contains(r.Warning, "memory snapshot") || r.Chain != "quiesce,memory,zfs" || r.Strict {
				t.Errorf("vm10: %+v", r)
			}
		case 11:
			if r.Strategy != StrategyESXiQuiesce || r.Warning != "" {
				t.Errorf("vm11: %+v", r)
			}
		}
	}
	// no plain snapshot may have been tried for vm10
	for _, c := range tr.calls {
		if strings.HasPrefix(c, "create 10 mem=false q=false") {
			t.Errorf("plain step is not in the chain: %v", tr.calls)
		}
	}
}

func TestESXiChainZFSOnly(t *testing.T) {
	withPolicy(t, "h:vm10,zfs\n", "h")
	tr := newFakeTr()
	f, _ := NewESXiFreezer(tr, "", "s")
	Register(f)
	defer unregisterLast()
	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	defer sess.Thaw()
	for _, r := range sess.Results() {
		if r.VMID == 10 && (r.Frozen || r.Strategy != StrategyZFSOnly || r.SnapID != "") {
			t.Errorf("zfs-only vm: %+v", r)
		}
		if r.VMID == 11 && !r.Frozen {
			t.Errorf("vm11: %+v", r)
		}
	}
	for _, c := range tr.calls {
		if strings.HasPrefix(c, "create 10 ") {
			t.Errorf("a snapshot was taken for the zfs-only vm: %v", tr.calls)
		}
	}
	if v := StrictViolations(sess.Results()); len(v) != 0 {
		t.Errorf("zfs-only is not a violation: %v", v)
	}
}

func TestESXiChainStrict(t *testing.T) {
	withPolicy(t, "[quiesce,memory]\n", "h")
	tr := newFakeTr()
	tr.allFail[10] = true
	f, _ := NewESXiFreezer(tr, "", "s")
	Register(f)
	defer unregisterLast()
	sess := FreezeAll(esxiGuests(10, 11), 5*time.Second)
	defer sess.Thaw()
	v := StrictViolations(sess.Results())
	if len(v) != 1 || v[0] != 10 {
		t.Errorf("violations: %v (%+v)", v, sess.Results())
	}
}

func TestESXiChainStepTimeoutThenFallback(t *testing.T) {
	// memory hangs on vm10 (fake: create blocks until its context ends) - the
	// step timeout of 1s ends it and the plain step is not reached because the
	// fake keeps hanging for every create of vm10, so the guest ends up unfrozen.
	withPolicy(t, "[memory:1,zfs]\n", "h")
	tr := newFakeTr()
	tr.hang[10] = true
	f, _ := NewESXiFreezer(tr, "", "s")
	Register(f)
	defer unregisterLast()
	start := time.Now()
	sess := FreezeAll(esxiGuests(10), time.Minute) // tool default is a minute, the step says 1s
	defer sess.Thaw()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("step timeout ignored: %v", d)
	}
	r := sess.Results()[0]
	if r.Frozen || !strings.Contains(r.Warning, "timed out after 1s") {
		t.Errorf("result: %+v", r)
	}
	if len(StrictViolations(sess.Results())) != 0 {
		t.Error("chain ends with zfs: no violation")
	}
}

func TestESXiChainReapsLeftover(t *testing.T) {
	// a snapshot with our name that appeared although the step failed is removed again
	withPolicy(t, "[quiesce,plain,zfs]\n", "h")
	tr := newFakeTr()
	tr.quiesceF[10] = true
	f, _ := NewESXiFreezer(tr, "", "s")
	name := f.vmSnapName(Guest{VMID: 10})
	tr.snaps[10] = nil
	Register(f)
	defer unregisterLast()
	// simulate the late snapshot of the failed quiesce step
	tr.snaps[10] = append(tr.snaps[10], esxiSnapshot("99", name, ESXiTag))
	sess := FreezeAll(esxiGuests(10), 5*time.Second)
	defer sess.Thaw()
	n := 0
	for _, s := range tr.snaps[10] {
		if s.ID == "99" {
			n++
		}
	}
	if n != 0 {
		t.Errorf("leftover snapshot not reaped: %+v", tr.snaps[10])
	}
}

func TestQEMUChainBranches(t *testing.T) {
	q := &QEMUFreezer{SocketDir: t.TempDir(), QMPSocketDir: t.TempDir(), strategies: map[int]string{}}
	g := Guest{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU}

	withPolicy(t, "[zfs]\n", "")
	if s, err := q.Freeze(g, time.Second); s != StrategyZFSOnly || err != nil {
		t.Errorf("zfs only: %q %v", s, err)
	}
	withPolicy(t, "[memory,zfs]\n", "") // memory does not exist on Proxmox
	if s, err := q.Freeze(g, time.Second); s != "" || err == nil {
		t.Errorf("memory on proxmox: %q %v", s, err)
	}
	withPolicy(t, "[quiesce,pause,zfs,2]\n", "") // no sockets: nothing works, not an error
	if s, err := q.Freeze(g, time.Second); s != StrategyNone || err != nil {
		t.Errorf("no sockets: %q %v", s, err)
	}
	if c := q.ChainOf(g); c.String() != "quiesce,pause,zfs,2" {
		t.Errorf("chain: %s", c)
	}
}

func TestDefaultChains(t *testing.T) {
	if c := DefaultChainFor(PlatformProxmoxLXC, TypeLXC); c.String() != "quiesce,zfs" {
		t.Errorf("lxc: %s", c)
	}
	if c := DefaultChainFor(PlatformProxmoxQEMU, TypeVM); c.String() != "quiesce,pause,zfs" {
		t.Errorf("qemu: %s", c)
	}
	if c := DefaultChainFor(PlatformESXi, TypeVM); c.String() != "quiesce,plain,zfs" {
		t.Errorf("esxi: %s", c)
	}
}
