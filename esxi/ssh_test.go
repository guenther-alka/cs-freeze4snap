package esxi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeHost is an in-process sshd that answers like an ESXi shell (vim-cmd, esxcli).
type fakeHost struct {
	mu    sync.Mutex
	snaps map[int][]Snapshot
	next  int
	fp    string
	addr  string
	fail  string // vim-cmd fault text for snapshot.create when set
}

var reCreate = regexp.MustCompile(`^vim-cmd vmsvc/snapshot\.create (\d+) '([^']*)' '([^']*)' (\d) (\d)$`)

func (h *fakeHost) exec(cmd string) (string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case cmd == "vim-cmd vmsvc/getallvms":
		return sampleGetAllVMs, 0
	case cmd == "esxcli storage nfs list":
		return sampleNFSList, 0
	case cmd == "esxcli storage nfs41 list":
		return "esxcli: unknown namespace\n", 1
	case strings.HasPrefix(cmd, `echo "@@`):
		var id int
		fmt.Sscanf(cmd, `echo "@@%d"`, &id)
		// real format: one "name" line, datastores in padded columns
		switch id {
		case 10:
			return "@@10\nPowered on\nname                 192.168.2.203_nvme_nfs            local_datastore                                  \n", 0
		case 25:
			return "@@25\nPowered off\nname                 nfs2                           \n", 0
		}
		return fmt.Sprintf("@@%d\nPowered on\nname                 local_datastore                                  \n", id), 0
	case strings.HasPrefix(cmd, "vim-cmd vmsvc/snapshot.get "):
		id, _ := strconv.Atoi(strings.Fields(cmd)[2])
		out := "Get Snapshot:\n"
		for i, s := range h.snaps[id] {
			pre := "--"
			if i > 0 {
				pre = "----"
			}
			out += fmt.Sprintf("%sSnapshot Name        : %s\n%sSnapshot Id        : %s\n%sSnapshot Desciption  : %s\n", pre, s.Name, pre, s.ID, pre, s.Desc)
		}
		return out, 0
	case reCreate.MatchString(cmd):
		m := reCreate.FindStringSubmatch(cmd)
		if h.fail != "" {
			return "(vim.fault.ToolsUnavailable) {\n   msg = \"" + h.fail + "\"\n}\n", 1
		}
		id, _ := strconv.Atoi(m[1])
		h.next++
		h.snaps[id] = append(h.snaps[id], Snapshot{ID: strconv.Itoa(h.next), Name: m[2], Desc: m[3]})
		return "Create Snapshot:\n", 0
	case strings.HasPrefix(cmd, "vim-cmd vmsvc/snapshot.remove "):
		f := strings.Fields(cmd)
		id, _ := strconv.Atoi(f[2])
		var keep []Snapshot
		for _, s := range h.snaps[id] {
			if s.ID != f[3] {
				keep = append(keep, s)
			}
		}
		h.snaps[id] = keep
		return "Remove Snapshot:\n", 0
	}
	return "unknown command: " + cmd + "\n", 127
}

func startFakeHost(t *testing.T, password string) *fakeHost {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{snaps: map[int][]Snapshot{}, fp: ssh.FingerprintSHA256(signer.PublicKey())}
	cfg := &ssh.ServerConfig{
		// like ESXi: passwords only via keyboard-interactive
		KeyboardInteractiveCallback: func(_ ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			ans, err := ch("", "", []string{"Password: "}, []bool{false})
			if err != nil || len(ans) != 1 || ans[0] != password {
				return nil, fmt.Errorf("denied")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					ch, creqs, _ := nc.Accept()
					go func() {
						defer ch.Close()
						for r := range creqs {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var p struct{ Cmd string }
							ssh.Unmarshal(r.Payload, &p)
							r.Reply(true, nil)
							out, rc := h.exec(p.Cmd)
							ch.Write([]byte(out))
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{uint32(rc)}))
							ch.CloseWrite() // like OpenSSH: exit-status, EOF, close
							return
						}
					}()
				}
			}()
		}
	}()
	return h
}

func (h *fakeHost) cfg() Config {
	host, port, _ := net.SplitHostPort(h.addr)
	p, _ := strconv.Atoi(port)
	return Config{Host: host, Port: p, Proto: "ssh", User: "root", Password: "pw", HostKey: h.fp}
}

func TestSSHTransport(t *testing.T) {
	h := startFakeHost(t, "pw")
	tr, err := Open(h.cfg())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx := context.Background()

	// VM 10 has a second datastore in the same get.datastores line: skipped ...
	d, err := DiscoverNFS(ctx, tr, DiscoverOpts{Path: "/nvme/nfs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.VMs) != 0 || len(d.Skipped) != 1 || d.Skipped[0].VMID != 10 || !strings.Contains(d.Skipped[0].Reason, "local_datastore") {
		t.Fatalf("mixed vm not skipped: %+v", d)
	}
	// ... unless mixed VMs are allowed
	d, err = DiscoverNFS(ctx, tr, DiscoverOpts{Path: "/nvme/nfs", AllowMixed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.VMs) != 1 || d.VMs[0].ID != 10 || len(d.Skipped) != 0 || len(d.Warnings) != 1 {
		t.Fatalf("discovery: %+v", d)
	}

	id, err := tr.CreateSnapshot(ctx, 10, "cs4s-test-10", "cs-freeze4snap", false, true)
	if err != nil || id == "" {
		t.Fatalf("create: %q %v", id, err)
	}
	s, _ := tr.Snapshots(ctx, 10)
	if len(s) != 1 || s[0].Name != "cs4s-test-10" || s[0].Desc != "cs-freeze4snap" {
		t.Fatalf("snapshots: %+v", s)
	}
	if err := tr.RemoveSnapshot(ctx, 10, id); err != nil {
		t.Fatal(err)
	}
	if s, _ := tr.Snapshots(ctx, 10); len(s) != 0 {
		t.Fatalf("snapshot not removed: %+v", s)
	}
	if err := tr.RemoveSnapshot(ctx, 10, "1; reboot"); err == nil {
		t.Error("injection in snapshot id must be refused")
	}
}

func TestSSHCreateFault(t *testing.T) {
	h := startFakeHost(t, "pw")
	h.fail = "VMware Tools is not running"
	tr, err := Open(h.cfg())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if _, err := tr.CreateSnapshot(context.Background(), 10, "n", "d", false, true); err == nil || !strings.Contains(err.Error(), "Tools") {
		t.Fatalf("want fault error, got %v", err)
	}
}

func TestSSHAuthAndHostKey(t *testing.T) {
	h := startFakeHost(t, "pw")

	c := h.cfg()
	c.Password = "wrong"
	if _, err := Open(c); err == nil {
		t.Error("wrong password accepted")
	}

	c = h.cfg()
	c.HostKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := Open(c); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("want host key mismatch, got %v", err)
	}

	c = h.cfg()
	c.HostKey = "" // unpinned: works, but warns and names the fingerprint
	tr, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	w := tr.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], h.fp) {
		t.Errorf("warnings: %v", w)
	}
}
