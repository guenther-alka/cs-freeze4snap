package freezer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// envQGASocketDir / envQMPSocketDir override the default Proxmox socket
// locations when set. Mainly useful for testing against fake servers, but
// also a legitimate escape hatch for non-standard Proxmox installs.
const (
	envQGASocketDir = "CS_FREEZE4SNAP_QGA_DIR"
	envQMPSocketDir = "CS_FREEZE4SNAP_QMP_DIR"
)

// Strategy identifiers reported in GuestResult.Strategy.
const (
	StrategyQGA      = "qga"       // guest-agent fsfreeze - best case, only I/O paused
	StrategyQMPPause = "qmp-pause" // QMP stop/cont - whole vCPU paused, no guest cooperation needed
	StrategyNone     = "none"      // neither worked - snapshotting as-is (crash-consistent only)
	StrategyZFSOnly  = "zfs-only"  // the chain names no freeze step (chain "zfs"): ZFS snapshot only
	StrategyStopped  = "stopped"   // the VM is not running (no QMP socket): consistent as it stands
	// StrategyQMMem (qm.go) is the qm snapshot with RAM state ("memory" step).
)

// QEMUFreezer freezes/thaws QEMU/KVM VMs, trying progressively weaker but
// more universally available strategies:
//
//  1. QEMU Guest Agent (QGA) fsfreeze - requires qemu-guest-agent installed
//     and running *inside* the guest. Best case: only writes are paused,
//     the guest OS keeps running and flushes its own journal first.
//
//  2. QMP stop/cont - the hypervisor control socket that exists for every
//     running QEMU/KVM VM regardless of guest OS or in-guest tooling. This
//     pauses ALL vCPU execution (not just writes), which is heavier
//     (network connections etc. also pause briefly) but works universally
//     and gives at least the same consistency as a power-loss recovery
//     point, often better since in-flight writes complete before the
//     pause takes effect.
//
//  3. If even QMP is unreachable (e.g. the VM isn't actually running),
//     Freeze does NOT return an error - it returns strategy "none" with a
//     nil error, so the caller proceeds to snapshot the VM exactly as it
//     currently is (crash-consistent at best) rather than aborting the
//     whole operation. The goal is "freeze if at all possible, otherwise
//     snapshot anyway" - never let the absence of a guest agent block a
//     backup entirely.
//
// Both QGA and QMP protocols are implemented directly (newline-delimited
// JSON-RPC over their respective unix sockets) rather than pulling in an
// external library, to keep this tool dependency-free and consistent with
// the KISS approach used elsewhere in the project.
type QEMUFreezer struct {
	// SocketDir overrides the default Proxmox QGA socket directory
	// (/var/run/qemu-server), mainly for testing.
	SocketDir string
	// QMPSocketDir overrides the default Proxmox QMP socket directory
	// (also /var/run/qemu-server, different file extension), mainly for
	// testing.
	QMPSocketDir string

	mu         sync.Mutex
	strategies map[int]string // vmid -> strategy actually used, for Thaw dispatch
	memSnaps   map[int]string // vmid -> qm memory snapshot taken in this run
	notes      map[int]string // vmid -> why a later step had to take over
}

func init() {
	Register(&QEMUFreezer{strategies: make(map[int]string), memSnaps: make(map[int]string), notes: make(map[int]string)})
}

func (q *QEMUFreezer) Name() string { return "qemu" }

func (q *QEMUFreezer) Supports(g Guest) bool {
	return g.Type == TypeVM && g.Platform == PlatformProxmoxQEMU
}

func (q *QEMUFreezer) setStrategy(vmid int, strategy string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.strategies == nil {
		q.strategies = make(map[int]string)
	}
	q.strategies[vmid] = strategy
}

func (q *QEMUFreezer) getStrategy(vmid int) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, ok := q.strategies[vmid]
	return s, ok
}

func (q *QEMUFreezer) qgaSocketPath(vmid int) string {
	dir := q.SocketDir
	if dir == "" {
		dir = os.Getenv(envQGASocketDir)
	}
	if dir == "" {
		dir = "/var/run/qemu-server"
	}
	return fmt.Sprintf("%s/%d.qga", dir, vmid)
}

func (q *QEMUFreezer) qmpSocketPath(vmid int) string {
	dir := q.QMPSocketDir
	if dir == "" {
		dir = os.Getenv(envQMPSocketDir)
	}
	if dir == "" {
		dir = "/var/run/qemu-server"
	}
	return fmt.Sprintf("%s/%d.qmp", dir, vmid)
}

// ChainOf is the chain that applies to g: policy first, then freeze,pause.
func (q *QEMUFreezer) ChainOf(g Guest) Chain { return ChainFor(g, DefaultProxmoxChain) }

// Freeze walks the guest's chain (see policy.go): freeze = QGA fsfreeze, pause
// = QMP stop, memory = qm snapshot with RAM state (kept, see qm.go), each with
// its own timeout; the first that works wins.
func (q *QEMUFreezer) Freeze(g Guest, timeout time.Duration) (string, error) {
	// memory snapshots whose ZFS snapshot is gone (retention) are dropped first
	pruneMem(g, "", false)
	ch := q.ChainOf(g)
	steps, unsupported := ch.StepsFor(PlatformProxmoxQEMU)
	if len(steps) == 0 {
		if ch.ZFS && len(unsupported) == 0 {
			return StrategyZFSOnly, nil
		}
		return "", fmt.Errorf("chain %q has no step that works on a Proxmox VM (freeze, pause)", ch)
	}
	// a VM that is not running has nothing to freeze and is consistent as it
	// stands - even a strict chain (no zfs) must not stop the run for it
	if _, err := os.Stat(q.qmpSocketPath(g.VMID)); err != nil && errors.Is(err, fs.ErrNotExist) {
		q.setStrategy(g.VMID, StrategyStopped)
		q.mu.Lock()
		if q.notes == nil {
			q.notes = make(map[int]string)
		}
		q.notes[g.VMID] = "vm is not running - nothing to freeze, consistent as it stands"
		q.mu.Unlock()
		return StrategyStopped, nil
	}
	var fails []string
	for i, st := range steps {
		to := ch.StepTimeout(st, timeout)
		switch st.Kind {
		case StepQuiesce:
			err := q.freezeViaQGA(g.VMID, to)
			if err == nil {
				q.setStrategy(g.VMID, StrategyQGA)
				return StrategyQGA, nil
			}
			fails = append(fails, "freeze: "+err.Error())
		case StepPause:
			err := q.pauseViaQMP(g.VMID, to)
			if err == nil {
				q.setStrategy(g.VMID, StrategyQMPPause)
				return StrategyQMPPause, nil
			}
			fails = append(fails, "pause: "+err.Error())
		case StepMemory:
			if st.Timeout == 0 && ch.Timeout == 0 && to < defaultMemTimeout {
				to = defaultMemTimeout
			}
			name, err := memSnapshot(g, q.qmpSocketPath(g.VMID), to)
			if err == nil {
				q.setStrategy(g.VMID, StrategyQMMem)
				q.mu.Lock()
				if q.memSnaps == nil {
					q.memSnaps = make(map[int]string)
				}
				q.memSnaps[g.VMID] = name
				if i > 0 {
					if q.notes == nil {
						q.notes = make(map[int]string)
					}
					q.notes[g.VMID] = fmt.Sprintf("%s - took a memory snapshot instead: RAM state kept in %s (qm rollback %d %s)", strings.Join(fails, "; "), name, g.VMID, name)
				}
				q.mu.Unlock()
				return StrategyQMMem, nil
			}
			fails = append(fails, "memory: "+err.Error())
		}
	}

	// No step worked. This is deliberately NOT an error - see the Freezer
	// interface doc on the "none" strategy. The most common real-world
	// cause is simply "no qemu-guest-agent configured", which is a normal,
	// expected state for many VMs. Whether the run goes on is up to the chain
	// (strict chains abort in the caller, see StrictViolations).
	q.setStrategy(g.VMID, StrategyNone)
	return StrategyNone, nil
}

func (q *QEMUFreezer) Thaw(g Guest) error {
	strategy, _ := q.getStrategy(g.VMID)
	switch strategy {
	case StrategyQGA:
		return q.thawViaQGA(g.VMID)
	case StrategyQMPPause:
		return q.resumeViaQMP(g.VMID)
	case StrategyQMMem:
		// The VM snapshot stays (qm.go): it is the consistent restore point of
		// the ZFS snapshot. Only what is no longer needed is removed.
		q.mu.Lock()
		name := q.memSnaps[g.VMID]
		q.mu.Unlock()
		_, errs := pruneMem(g, name, true)
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	default:
		// StrategyNone (or unknown/never frozen) - nothing to undo.
		return nil
	}
}

// Note explains a degraded freeze (used for the Warning field).
func (q *QEMUFreezer) Note(g Guest) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.notes[g.VMID]
}

// --- QGA (guest agent) path ---

func (q *QEMUFreezer) freezeViaQGA(vmid int, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", q.qgaSocketPath(vmid), timeout)
	if err != nil {
		return fmt.Errorf("connect to qga socket: %w", err)
	}
	defer conn.Close()

	// Fail fast if the guest agent isn't actually responding, rather than
	// letting guest-fsfreeze-freeze itself hang for the full timeout.
	if err := jsonRPCCall(conn, timeout, `{"execute":"guest-ping"}`, nil); err != nil {
		return fmt.Errorf("qga ping failed (is qemu-guest-agent running in the guest?): %w", err)
	}
	var resp struct {
		Return int `json:"return"`
	}
	if err := jsonRPCCall(conn, timeout, `{"execute":"guest-fsfreeze-freeze"}`, &resp); err != nil {
		return fmt.Errorf("guest-fsfreeze-freeze failed: %w", err)
	}
	return nil
}

func (q *QEMUFreezer) thawViaQGA(vmid int) error {
	const thawTimeout = 10 * time.Second
	conn, err := net.DialTimeout("unix", q.qgaSocketPath(vmid), thawTimeout)
	if err != nil {
		return fmt.Errorf("guest-fsfreeze-thaw: cannot reach qga for vmid %d: %w", vmid, err)
	}
	defer conn.Close()

	var resp struct {
		Return int `json:"return"`
	}
	if err := jsonRPCCall(conn, thawTimeout, `{"execute":"guest-fsfreeze-thaw"}`, &resp); err != nil {
		return fmt.Errorf("guest-fsfreeze-thaw failed for vmid %d: %w", vmid, err)
	}
	return nil
}

// --- QMP (hypervisor control socket) path ---

func (q *QEMUFreezer) pauseViaQMP(vmid int, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", q.qmpSocketPath(vmid), timeout)
	if err != nil {
		return fmt.Errorf("connect to qmp socket: %w", err)
	}
	defer conn.Close()

	if err := qmpHandshake(conn, timeout); err != nil {
		return fmt.Errorf("qmp handshake: %w", err)
	}
	if err := jsonRPCCall(conn, timeout, `{"execute":"stop"}`, nil); err != nil {
		return fmt.Errorf("qmp stop failed: %w", err)
	}
	return nil
}

func (q *QEMUFreezer) resumeViaQMP(vmid int) error {
	const resumeTimeout = 10 * time.Second
	conn, err := net.DialTimeout("unix", q.qmpSocketPath(vmid), resumeTimeout)
	if err != nil {
		return fmt.Errorf("qmp cont: cannot reach qmp for vmid %d: %w", vmid, err)
	}
	defer conn.Close()

	if err := qmpHandshake(conn, resumeTimeout); err != nil {
		return fmt.Errorf("qmp handshake for cont (vmid %d): %w", vmid, err)
	}
	if err := jsonRPCCall(conn, resumeTimeout, `{"execute":"cont"}`, nil); err != nil {
		return fmt.Errorf("qmp cont failed for vmid %d: %w", vmid, err)
	}
	return nil
}

// qmpHandshake performs the mandatory QMP negotiation: the server sends a
// greeting banner unprompted on connect, and the client must reply with
// qmp_capabilities before any other command is accepted.
func qmpHandshake(conn net.Conn, timeout time.Duration) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	// Discard the greeting banner ({"QMP": {"version": ...}}).
	if _, err := reader.ReadBytes('\n'); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if _, err := conn.Write([]byte(`{"execute":"qmp_capabilities"}` + "\n")); err != nil {
		return fmt.Errorf("write qmp_capabilities: %w", err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read qmp_capabilities response: %w", err)
	}
	if err := checkJSONRPCError(line); err != nil {
		return err
	}
	return nil
}

// --- shared newline-delimited JSON-RPC helpers (used by both QGA and QMP,
// which happen to share the same wire framing: one JSON object per line) ---

func jsonRPCCall(conn net.Conn, timeout time.Duration, request string, out interface{}) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(request + "\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if err := checkJSONRPCError(line); err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(line, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func checkJSONRPCError(line []byte) error {
	var errResp struct {
		Error *struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &errResp); err == nil && errResp.Error != nil {
		return fmt.Errorf("rpc error: %s: %s", errResp.Error.Class, errResp.Error.Desc)
	}
	return nil
}
