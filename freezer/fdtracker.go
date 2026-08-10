//go:build linux

package freezer

import "sync"

// fdTracker holds open file descriptors keyed by VMID between a successful
// Freeze() and the matching Thaw(). Needed because LXCFreezer is a single
// shared instance used concurrently across multiple guests (one goroutine
// per guest during the parallel freeze phase in main.go), so state cannot
// simply live in a local variable.
type fdTracker struct {
	mu  sync.Mutex
	fds map[int]int
}

var freezeHandles = &fdTracker{fds: make(map[int]int)}

func (t *fdTracker) set(vmid, fd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fds[vmid] = fd
}

func (t *fdTracker) get(vmid int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fd, ok := t.fds[vmid]
	return fd, ok
}

func (t *fdTracker) delete(vmid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fds, vmid)
}
