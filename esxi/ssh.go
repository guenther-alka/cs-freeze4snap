package esxi

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshMaxSessions keeps the number of parallel exec channels below the
// sshd MaxSessions default (10) - guests are frozen in parallel.
const sshMaxSessions = 5

type sshTransport struct {
	cli  *ssh.Client
	sem  chan struct{}
	mu   sync.Mutex
	warn []string
}

func openSSH(c Config) (*sshTransport, error) {
	var auth []ssh.AuthMethod
	if c.Key != "" {
		pem, err := os.ReadFile(c.Key)
		if err != nil {
			return nil, fmt.Errorf("ssh key: %w", err)
		}
		var signer ssh.Signer
		if c.KeyPass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(c.KeyPass))
		} else {
			signer, err = ssh.ParsePrivateKey(pem)
		}
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w (OpenSSH format needed, PuTTY .ppk cannot be read)", c.Key, err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if c.Password != "" {
		pw := c.Password
		auth = append(auth, ssh.Password(pw))
		// ESXi offers passwords as keyboard-interactive only
		auth = append(auth, ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			a := make([]string, len(qs))
			for i := range a {
				a[i] = pw
			}
			return a, nil
		}))
	}
	t := &sshTransport{sem: make(chan struct{}, sshMaxSessions)}
	cb := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if c.HostKey == "" {
			t.addWarn("ssh host key not pinned (accepted " + fp + ") - set hostkey=" + fp + " in the cfg file")
			return nil
		}
		if !strings.EqualFold(strings.TrimSpace(c.HostKey), fp) {
			return fmt.Errorf("ssh host key mismatch: server presented %s, cfg pins %s", fp, c.HostKey)
		}
		return nil
	}
	cli, err := ssh.Dial("tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), &ssh.ClientConfig{
		User: c.User, Auth: auth, HostKeyCallback: cb, Timeout: c.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s: %w", c.User, c.Host, err)
	}
	t.cli = cli
	return t, nil
}

func (t *sshTransport) addWarn(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.warn = append(t.warn, s)
}

func (t *sshTransport) Name() string { return "ssh" }
func (t *sshTransport) Warnings() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.warn...)
}
func (t *sshTransport) Close() error { return t.cli.Close() }

// sq single-quotes s for the remote shell.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// run executes one remote command and returns stdout+stderr.
func (t *sshTransport) run(ctx context.Context, cmd string) (string, error) {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if debugOn {
		start := time.Now()
		defer func() { debugf("ssh run (%s): %v", cmd, time.Since(start).Round(time.Millisecond)) }()
	}
	sess, err := t.cli.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	buf := &lockedBuf{}
	sess.Stdout, sess.Stderr = buf, buf
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if debugOn {
			o := buf.String()
			if len(o) > 4000 {
				o = o[:4000] + "..."
			}
			debugf("output:\n%s", o)
		}
		if err != nil {
			return buf.String(), fmt.Errorf("%s: %w (%s)", firstWord(cmd), err, strings.TrimSpace(buf.String()))
		}
		return buf.String(), nil
	case <-ctx.Done():
		sess.Close()
		return buf.String(), ctx.Err()
	}
}

// lockedBuf collects stdout and stderr of a session into one buffer. Both
// streams are copied by separate goroutines, and a bare bytes.Buffer must not
// be used for that: its ReadFrom would race and lose output.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	if len(f) > 1 {
		return f[0] + " " + f[1]
	}
	return f[0]
}

func (t *sshTransport) Datastores(ctx context.Context) ([]Datastore, error) {
	out, err := t.run(ctx, "esxcli storage nfs list")
	if err != nil {
		return nil, err
	}
	ds := parseNFSList(out, "NFS")
	// NFS 4.1 datastores - the namespace is missing on very old hosts, ignore errors
	if out41, err := t.run(ctx, "esxcli storage nfs41 list"); err == nil {
		ds = append(ds, parseNFSList(out41, "NFS41")...)
	}
	return ds, nil
}

func (t *sshTransport) VMs(ctx context.Context) ([]VM, error) {
	out, err := t.run(ctx, "vim-cmd vmsvc/getallvms")
	if err != nil {
		return nil, err
	}
	vms := parseGetAllVMs(out)
	if len(vms) == 0 {
		return nil, nil
	}
	// power state and all datastores of every VM: one short command per VM, run
	// in parallel (each vim-cmd call costs ~0.7s on the host, 16 VMs in one
	// sequential script took 25s)
	type res struct {
		i   int
		vm  *VM
		err error
	}
	ch := make(chan res, len(vms))
	for i := range vms {
		go func(i int) {
			id := vms[i].ID // int: safe to put into a command
			out, err := t.run(ctx, fmt.Sprintf(`echo "@@%d"; vim-cmd vmsvc/power.getstate %d | tail -n 1; vim-cmd vmsvc/get.datastores %d | grep -E '^name '`, id, id, id))
			if err != nil {
				ch <- res{i: i, err: err}
				return
			}
			ch <- res{i: i, vm: parseLoop(out)[id]}
		}(i)
	}
	var firstErr error
	for range vms {
		r := <-ch
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("vm %d: %w", vms[r.i].ID, r.err)
			}
			continue
		}
		if r.vm != nil {
			vms[r.i].PowerState = r.vm.PowerState
			if len(r.vm.Datastores) > 0 {
				vms[r.i].Datastores = r.vm.Datastores
			}
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return vms, nil
}

func (t *sshTransport) Snapshots(ctx context.Context, vmid int) ([]Snapshot, error) {
	out, err := t.run(ctx, "vim-cmd vmsvc/snapshot.get "+strconv.Itoa(vmid))
	if err != nil {
		return nil, err
	}
	return parseSnapshotGet(out), nil
}

func b2i(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// vimFault reports whether vim-cmd printed a fault instead of doing the job.
func vimFault(out string) bool {
	return strings.Contains(out, "(vim.fault.") || strings.Contains(out, "Fault cause") ||
		strings.Contains(out, "Failed to") || strings.Contains(out, "failed")
}

func (t *sshTransport) CreateSnapshot(ctx context.Context, vmid int, name, desc string, mem, quiesce bool) (string, error) {
	cmd := fmt.Sprintf("vim-cmd vmsvc/snapshot.create %d %s %s %s %s", vmid, sq(name), sq(desc), b2i(mem), b2i(quiesce))
	out, err := t.run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if vimFault(out) || !strings.Contains(out, "Create Snapshot") {
		return "", fmt.Errorf("snapshot.create vm %d: %s", vmid, strings.TrimSpace(out))
	}
	// vim-cmd does not print the new snapshot id: find it by its unique name
	snaps, err := t.Snapshots(ctx, vmid)
	if err != nil {
		return "", err
	}
	id := ""
	for _, s := range snaps {
		if s.Name == name {
			id = s.ID // last one wins if the name was reused
		}
	}
	if id == "" {
		return "", fmt.Errorf("snapshot %q on vm %d was created but not found afterwards", name, vmid)
	}
	return id, nil
}

func (t *sshTransport) RemoveSnapshot(ctx context.Context, vmid int, snapID string) error {
	if _, err := strconv.Atoi(snapID); err != nil {
		return fmt.Errorf("bad snapshot id %q", snapID)
	}
	out, err := t.run(ctx, fmt.Sprintf("vim-cmd vmsvc/snapshot.remove %d %s 0", vmid, snapID))
	if err != nil {
		return err
	}
	if vimFault(out) || !strings.Contains(out, "Remove Snapshot") {
		return fmt.Errorf("snapshot.remove vm %d snapshot %s: %s", vmid, snapID, strings.TrimSpace(out))
	}
	return nil
}
