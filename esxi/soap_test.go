package esxi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVSphere answers the SOAP calls the transport makes, the way ESXi 8 does.
// Write calls are refused unless the User-Agent starts with "VMware" (free license).
type fakeVSphere struct {
	mu     sync.Mutex
	srv    *httptest.Server
	snaps  map[int][]Snapshot
	tasks  map[string]string // task -> result moref, "" = not yet polled
	polled map[string]int
	next   int
	uas    []string
	hang   bool            // tasks stay "running" until cancelled
	cancel map[string]bool // CancelTask received
}

const envOpen = `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>`
const envClose = `</soapenv:Body></soapenv:Envelope>`

func vfault(msg, typ string) string {
	return envOpen + `<soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>` + msg + `</faultstring><detail><` + typ + `Fault xmlns="urn:vim25" xsi:type="` + typ + `"></` + typ + `Fault></detail></soapenv:Fault>` + envClose
}

func prop(name, val string) string {
	return `<propSet><name>` + name + `</name><val xsi:type="xsd:string">` + val + `</val></propSet>`
}

var (
	reVMObj  = regexp.MustCompile(`<obj type="VirtualMachine">(\d+)</obj>`)
	reSnapOb = regexp.MustCompile(`type="VirtualMachineSnapshot">([^<]+)<`)
	reTaskOb = regexp.MustCompile(`<obj type="Task">([^<]+)</obj>`)
	reThisVM = regexp.MustCompile(`<_this type="VirtualMachine">(\d+)</_this>`)
	reName   = regexp.MustCompile(`<name>([^<]*)</name>`)
	reDesc   = regexp.MustCompile(`<description>([^<]*)</description>`)
)

func (f *fakeVSphere) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uas = append(f.uas, r.UserAgent())
	write := strings.Contains(body, "_Task xmlns") && !strings.Contains(body, "RetrieveProperties")
	out := func(s string) { w.Header().Set("Content-Type", "text/xml"); io.WriteString(w, envOpen+s+envClose) }
	switch {
	case strings.Contains(body, "<RetrieveServiceContent"):
		out(`<RetrieveServiceContentResponse xmlns="urn:vim25"><returnval><rootFolder type="Folder">ha-folder-root</rootFolder><propertyCollector type="PropertyCollector">ha-property-collector</propertyCollector><sessionManager type="SessionManager">ha-sessionmgr</sessionManager><viewManager type="ViewManager">ViewManager</viewManager></returnval></RetrieveServiceContentResponse>`)
	case strings.Contains(body, "<Login "):
		if !strings.Contains(body, "<password>pw</password>") {
			w.WriteHeader(500)
			io.WriteString(w, vfault("Cannot complete login due to an incorrect user name or password.", "InvalidLogin"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "vmware_soap_session", Value: "abc", Path: "/"})
		out(`<LoginResponse xmlns="urn:vim25"><returnval><key>abc</key></returnval></LoginResponse>`)
	case strings.Contains(body, "<Logout "):
		out(`<LogoutResponse xmlns="urn:vim25"></LogoutResponse>`)
	case write && !strings.HasPrefix(r.UserAgent(), "VMware"):
		w.WriteHeader(500)
		io.WriteString(w, vfault("Current license or ESXi version prohibits execution of the requested operation.", "RestrictedVersion"))
	case strings.Contains(body, "<CreateContainerView"):
		v := "view-ds"
		if strings.Contains(body, "<type>VirtualMachine</type>") {
			v = "view-vm"
		}
		out(`<CreateContainerViewResponse xmlns="urn:vim25"><returnval type="ContainerView">` + v + `</returnval></CreateContainerViewResponse>`)
	case strings.Contains(body, `<obj type="ContainerView">view-ds</obj>`):
		ds := func(id, name, typ, host, path string) string {
			info := `<val xsi:type="VmfsDatastoreInfo"><name>` + name + `</name></val>`
			if host != "" {
				info = `<val xsi:type="NasDatastoreInfo"><name>` + name + `</name><nas><remoteHost>` + host + `</remoteHost><remotePath>` + path + `</remotePath></nas></val>`
			}
			return `<returnval><obj type="Datastore">` + id + `</obj>` + prop("name", name) + prop("summary.type", typ) + `<propSet><name>info</name>` + info + `</propSet></returnval>`
		}
		out(`<RetrievePropertiesResponse xmlns="urn:vim25">` + ds("ds-1", "nvme", "NFS", "192.168.2.203", "/nvme/nfs") + ds("ds-2", "nfs2", "NFS", "192.168.2.203", "/daten1/nfs") + ds("ds-3", "local", "VMFS", "", "") + `</RetrievePropertiesResponse>`)
	case strings.Contains(body, `<obj type="ContainerView">view-vm</obj>`):
		vm := func(id, name, power string, dss ...string) string {
			var refs string
			for _, d := range dss {
				refs += `<ManagedObjectReference type="Datastore" xsi:type="ManagedObjectReference">` + d + `</ManagedObjectReference>`
			}
			return `<returnval><obj type="VirtualMachine">` + id + `</obj>` + prop("name", name) + prop("runtime.powerState", power) + `<propSet><name>datastore</name><val xsi:type="ArrayOfManagedObjectReference">` + refs + `</val></propSet></returnval>`
		}
		out(`<RetrievePropertiesResponse xmlns="urn:vim25">` + vm("10", "web", "poweredOn", "ds-1") + vm("11", "off", "poweredOff", "ds-1") + vm("12", "mixed", "poweredOn", "ds-1", "ds-3") + vm("13", "other", "poweredOn", "ds-2") + `</RetrievePropertiesResponse>`)
	case strings.Contains(body, "<pathSet>snapshot</pathSet>"):
		id, _ := strconv.Atoi(reVMObj.FindStringSubmatch(body)[1])
		if len(f.snaps[id]) == 0 {
			out(`<RetrievePropertiesResponse xmlns="urn:vim25"></RetrievePropertiesResponse>`)
			return
		}
		var tree string
		for i := len(f.snaps[id]) - 1; i >= 0; i-- { // nested: root -> child -> child
			s := f.snaps[id][i]
			inner := tree
			tag := "childSnapshotList"
			if i == 0 {
				tag = "rootSnapshotList"
			}
			tree = `<` + tag + `><snapshot type="VirtualMachineSnapshot">` + s.ID + `</snapshot><name>` + s.Name + `</name><description>` + s.Desc + `</description>` + inner + `</` + tag + `>`
		}
		out(`<RetrievePropertiesResponse xmlns="urn:vim25"><returnval><obj type="VirtualMachine">` + strconv.Itoa(id) + `</obj><propSet><name>snapshot</name><val xsi:type="VirtualMachineSnapshotInfo">` + tree + `</val></propSet></returnval></RetrievePropertiesResponse>`)
	case strings.Contains(body, "<CreateSnapshot_Task"):
		id, _ := strconv.Atoi(reThisVM.FindStringSubmatch(body)[1])
		f.next++
		ref := fmt.Sprintf("%d-snapshot-%d", id, f.next)
		f.snaps[id] = append(f.snaps[id], Snapshot{ID: ref, Name: reName.FindStringSubmatch(body)[1], Desc: reDesc.FindStringSubmatch(body)[1]})
		task := fmt.Sprintf("haTask-%d-create", f.next)
		f.tasks[task] = ref
		out(`<CreateSnapshot_TaskResponse xmlns="urn:vim25"><returnval type="Task">` + task + `</returnval></CreateSnapshot_TaskResponse>`)
	case strings.Contains(body, "<RemoveSnapshot_Task"):
		ref := reSnapOb.FindStringSubmatch(body)[1]
		for id, l := range f.snaps {
			var keep []Snapshot
			for _, s := range l {
				if s.ID != ref {
					keep = append(keep, s)
				}
			}
			f.snaps[id] = keep
		}
		f.next++
		task := fmt.Sprintf("haTask-%d-remove", f.next)
		f.tasks[task] = "-"
		out(`<RemoveSnapshot_TaskResponse xmlns="urn:vim25"><returnval type="Task">` + task + `</returnval></RemoveSnapshot_TaskResponse>`)
	case strings.Contains(body, "<CancelTask"):
		task := regexp.MustCompile(`<_this type="Task">([^<]+)<`).FindStringSubmatch(body)[1]
		if f.cancel == nil {
			f.cancel = map[string]bool{}
		}
		f.cancel[task] = true
		out(`<CancelTaskResponse xmlns="urn:vim25"></CancelTaskResponse>`)
	case strings.Contains(body, `<obj type="Task">`):
		task := reTaskOb.FindStringSubmatch(body)[1]
		f.polled[task]++
		if f.hang {
			st := "running"
			if f.cancel[task] {
				st = "error"
			}
			out(`<RetrievePropertiesResponse xmlns="urn:vim25"><returnval><obj type="Task">` + task + `</obj>` + prop("info.state", st) + `</returnval></RetrievePropertiesResponse>`)
			return
		}
		if f.polled[task] == 1 { // first look: still running
			out(`<RetrievePropertiesResponse xmlns="urn:vim25"><returnval><obj type="Task">` + task + `</obj>` + prop("info.state", "running") + `</returnval></RetrievePropertiesResponse>`)
			return
		}
		res := ""
		if f.tasks[task] != "-" {
			res = `<propSet><name>info.result</name><val type="VirtualMachineSnapshot" xsi:type="ManagedObjectReference">` + f.tasks[task] + `</val></propSet>`
		}
		out(`<RetrievePropertiesResponse xmlns="urn:vim25"><returnval><obj type="Task">` + task + `</obj>` + prop("info.state", "success") + res + `</returnval></RetrievePropertiesResponse>`)
	default:
		w.WriteHeader(500)
		io.WriteString(w, vfault("fake: unhandled request", "NotImplemented"))
	}
}

func startFakeVSphere(t *testing.T) (*fakeVSphere, Config) {
	t.Helper()
	f := &fakeVSphere{snaps: map[int][]Snapshot{}, tasks: map[string]string{}, polled: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "https://"))
	p, _ := strconv.Atoi(port)
	return f, Config{Host: host, Port: p, Proto: "soap", User: "root", Password: "pw"}
}

func TestSOAPTransport(t *testing.T) {
	f, c := startFakeVSphere(t)
	tr, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx := context.Background()

	if w := tr.Warnings(); len(w) != 1 || !strings.Contains(w[0], "tls_sha256") {
		t.Errorf("want an unpinned-certificate warning, got %v", w)
	}

	d, err := DiscoverNFS(ctx, tr, DiscoverOpts{Path: "/nvme/nfs"})
	if err != nil {
		t.Fatal(err)
	}
	if ids(d.VMs) != "web" || len(d.Skipped) != 2 {
		t.Fatalf("discovery: vms %s skipped %+v", ids(d.VMs), d.Skipped)
	}
	if d.VMs[0].ID != 10 || d.VMs[0].Datastores[0] != "nvme" {
		t.Errorf("vm: %+v", d.VMs[0])
	}

	id, err := tr.CreateSnapshot(ctx, 10, "cs4s-test-10", "cs-freeze4snap", false, true)
	if err != nil || id != "10-snapshot-1" {
		t.Fatalf("create: %q %v", id, err)
	}
	s, err := tr.Snapshots(ctx, 10)
	if err != nil || len(s) != 1 || s[0].Name != "cs4s-test-10" || s[0].ID != id || s[0].Desc != "cs-freeze4snap" {
		t.Fatalf("snapshots: %+v %v", s, err)
	}
	if err := tr.RemoveSnapshot(ctx, 10, id); err != nil {
		t.Fatal(err)
	}
	if s, _ := tr.Snapshots(ctx, 10); len(s) != 0 {
		t.Fatalf("not removed: %+v", s)
	}
	for _, ua := range f.uas {
		if !strings.HasPrefix(ua, "VMware") {
			t.Fatalf("User-Agent %q does not identify as a VMware client", ua)
		}
	}
}

func TestSOAPLicenseHint(t *testing.T) {
	_, c := startFakeVSphere(t)
	c.UserAgent = "curl/8.0"
	tr, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	_, err = tr.CreateSnapshot(context.Background(), 10, "n", "d", false, false)
	if err == nil || !strings.Contains(err.Error(), "license") || !strings.Contains(err.Error(), "User-Agent") {
		t.Fatalf("want license fault with hint, got %v", err)
	}
}

// TestSOAPLoginAndPin needs one working loopback TLS connection to the
// in-process fake vSphere server: it asserts on the exact error strings
// ("incorrect user name or password", "certificate mismatch"), so a machine
// where something filters the loopback traffic of a freshly built test binary
// (AV/firewall) fails this test although the code is fine. That happened once
// on Windows and was mis-read as a platform difference; verified passing on
// Windows (20 consecutive runs) and on Linux, 2026-09-20.
func TestSOAPLoginAndPin(t *testing.T) {
	f, c := startFakeVSphere(t)
	bad := c
	bad.Password = "nope"
	if _, err := Open(bad); err == nil || !strings.Contains(err.Error(), "incorrect user name or password") {
		t.Errorf("want login failure, got %v", err)
	}

	sum := sha256.Sum256(f.srv.Certificate().Raw)
	good := c
	good.TLSSHA256 = strings.ToUpper(hex.EncodeToString(sum[:]))
	tr, err := Open(good)
	if err != nil {
		t.Fatalf("pinned cert rejected: %v", err)
	}
	if len(tr.Warnings()) != 0 {
		t.Errorf("no warning expected when pinned: %v", tr.Warnings())
	}
	tr.Close()

	wrong := c
	wrong.TLSSHA256 = strings.Repeat("00", 32)
	if _, err := Open(wrong); err == nil || !strings.Contains(err.Error(), "certificate mismatch") {
		t.Errorf("want certificate mismatch, got %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	p := t.TempDir() + "/c.cfg"
	if err := writeFile(p, "\xef\xbb\xbf# comment\nhost = 10.0.0.5\nproto=SOAP\npassword=a=b c\n\nport=8443\ntimeout=7\n"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Host != "10.0.0.5" || c.Proto != "soap" || c.Password != "a=b c" || c.Port != 8443 || c.User != "root" || c.Timeout.Seconds() != 7 {
		t.Errorf("cfg: %+v", c)
	}
	writeFile(p, "hots=1\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("typo in key must be an error, got %v", err)
	}
	if err := (&Config{}).Normalize(); err == nil {
		t.Error("empty config must not validate")
	}
}

func TestSOAPTimeoutCancelsTheTask(t *testing.T) {
	f, c := startFakeVSphere(t)
	tr, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	f.mu.Lock()
	f.hang = true
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := tr.CreateSnapshot(ctx, 10, "cs4s-x-10", "cs-freeze4snap", true, false); err == nil {
		t.Fatal("a hanging snapshot must time out")
	}
	f.mu.Lock()
	n := len(f.cancel)
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("CancelTask calls: %d", n)
	}
}
