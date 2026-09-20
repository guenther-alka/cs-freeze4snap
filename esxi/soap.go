package esxi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// userAgentDefault: a standalone ESXi with the free license refuses every
// write call (snapshots, power) from generic clients with
// "Current license or ESXi version prohibits execution of the requested
// operation", but accepts them from clients that identify as a VMware client.
// This is the string the classic vSphere Client sent. It is a workaround
// found by testing, VMware may change it - the ssh transport does not depend on it.
const userAgentDefault = "VMware VI Client/4.0.0"

// soapAction: API version 4.0 is understood by every ESXi up to 8.x.
const soapAction = `"urn:vim25/4.0"`

var reMoid = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

type soapTransport struct {
	url string
	hc  *http.Client
	ua  string

	sessionMgr, propColl, rootFolder, viewMgr string

	mu      sync.Mutex
	warn    []string
	dsNames map[string]string // datastore moid -> name
}

func openSOAP(c Config) (*soapTransport, error) {
	jar, _ := cookiejar.New(nil)
	t := &soapTransport{ua: c.UserAgent, url: "https://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) + "/sdk"}
	if t.ua == "" {
		t.ua = userAgentDefault
	}
	pin := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(c.TLSSHA256))
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // ESXi has a self-signed certificate; optional pin below
	if pin != "" {
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			sum := sha256.Sum256(raw[0])
			if got := hex.EncodeToString(sum[:]); got != pin {
				return fmt.Errorf("tls certificate mismatch: server presented sha256 %s, cfg pins %s", got, pin)
			}
			return nil
		}
	} else {
		t.addWarn("soap tls certificate not verified - set tls_sha256=<hex> in the cfg file to pin it")
	}
	t.hc = &http.Client{
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: (&net.Dialer{Timeout: c.Timeout}).DialContext, TLSHandshakeTimeout: c.Timeout},
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout+10*time.Second)
	defer cancel()
	sc, err := t.call(ctx, `<RetrieveServiceContent xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></RetrieveServiceContent>`)
	if err != nil {
		return nil, fmt.Errorf("soap %s: %w", t.url, err)
	}
	t.sessionMgr = sc.find("sessionManager").text()
	t.propColl = sc.find("propertyCollector").text()
	t.rootFolder = sc.find("rootFolder").text()
	t.viewMgr = sc.find("viewManager").text()
	if t.sessionMgr == "" || t.propColl == "" || t.rootFolder == "" || t.viewMgr == "" {
		return nil, fmt.Errorf("soap %s: unexpected ServiceContent (is this an ESXi/vCenter /sdk endpoint?)", t.url)
	}
	if _, err := t.call(ctx, `<Login xmlns="urn:vim25"><_this type="SessionManager">`+xesc(t.sessionMgr)+`</_this><userName>`+xesc(c.User)+`</userName><password>`+xesc(c.Password)+`</password></Login>`); err != nil {
		return nil, fmt.Errorf("soap login as %s: %w", c.User, err)
	}
	return t, nil
}

func (t *soapTransport) addWarn(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.warn = append(t.warn, s)
}

func (t *soapTransport) Name() string { return "soap" }
func (t *soapTransport) Warnings() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.warn...)
}

func (t *soapTransport) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = t.call(ctx, `<Logout xmlns="urn:vim25"><_this type="SessionManager">`+xesc(t.sessionMgr)+`</_this></Logout>`)
	t.hc.CloseIdleConnections()
	return nil
}

// call posts one SOAP body and returns the parsed envelope; a SOAP fault
// becomes an error.
func (t *soapTransport) call(ctx context.Context, body string) (*xnode, error) {
	env := `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>` + body + `</soapenv:Body></soapenv:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader([]byte(env)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", soapAction)
	req.Header.Set("User-Agent", t.ua)
	if debugOn {
		start := time.Now()
		defer func() { debugf("soap call: %v", time.Since(start).Round(time.Millisecond)) }()
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	root, perr := parseXML(b)
	if perr != nil {
		return nil, fmt.Errorf("http %d, not a SOAP answer: %.200s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if f := root.find("faultstring"); f != nil {
		msg := f.Text
		if d := root.find("detail"); d != nil && len(d.Kids) > 0 {
			msg += " [" + d.Kids[0].Name + "]"
		}
		if strings.Contains(msg, "license") {
			msg += " (free ESXi license: the client must send a VMware User-Agent, or use proto=ssh)"
		}
		return nil, errors.New(msg)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return root, nil
}

// obj is one object of a RetrieveProperties answer.
type obj struct {
	ID    string
	Props map[string]*xnode
}

func parseObjs(root *xnode) []obj {
	var out []obj
	for _, rv := range root.all("returnval") {
		o := obj{ID: rv.child("obj").text(), Props: map[string]*xnode{}}
		for _, ps := range rv.Kids {
			if ps.Name != "propSet" {
				continue
			}
			o.Props[ps.child("name").text()] = ps.child("val")
		}
		out = append(out, o)
	}
	return out
}

// listView returns all objects of one managed type with the given properties.
func (t *soapTransport) listView(ctx context.Context, typ string, paths ...string) ([]obj, error) {
	r, err := t.call(ctx, `<CreateContainerView xmlns="urn:vim25"><_this type="ViewManager">`+xesc(t.viewMgr)+`</_this><container type="Folder">`+xesc(t.rootFolder)+`</container><type>`+typ+`</type><recursive>true</recursive></CreateContainerView>`)
	if err != nil {
		return nil, err
	}
	view := r.find("returnval").text()
	if view == "" {
		return nil, errors.New("CreateContainerView returned no view")
	}
	var ps strings.Builder
	for _, p := range paths {
		ps.WriteString("<pathSet>" + p + "</pathSet>")
	}
	r, err = t.call(ctx, `<RetrieveProperties xmlns="urn:vim25"><_this type="PropertyCollector">`+xesc(t.propColl)+`</_this><specSet><propSet><type>`+typ+`</type>`+ps.String()+`</propSet><objectSet><obj type="ContainerView">`+xesc(view)+`</obj><skip>true</skip><selectSet xsi:type="TraversalSpec"><name>tv</name><type>ContainerView</type><path>view</path><skip>false</skip></selectSet></objectSet></specSet></RetrieveProperties>`)
	if err != nil {
		return nil, err
	}
	return parseObjs(r), nil
}

// getProps reads properties of one object.
func (t *soapTransport) getProps(ctx context.Context, typ, id string, paths ...string) (map[string]*xnode, error) {
	if !reMoid.MatchString(id) {
		return nil, fmt.Errorf("bad object id %q", id)
	}
	var ps strings.Builder
	for _, p := range paths {
		ps.WriteString("<pathSet>" + p + "</pathSet>")
	}
	r, err := t.call(ctx, `<RetrieveProperties xmlns="urn:vim25"><_this type="PropertyCollector">`+xesc(t.propColl)+`</_this><specSet><propSet><type>`+typ+`</type>`+ps.String()+`</propSet><objectSet><obj type="`+typ+`">`+id+`</obj></objectSet></specSet></RetrieveProperties>`)
	if err != nil {
		return nil, err
	}
	os := parseObjs(r)
	if len(os) == 0 {
		return map[string]*xnode{}, nil
	}
	return os[0].Props, nil
}

func (t *soapTransport) Datastores(ctx context.Context) ([]Datastore, error) {
	objs, err := t.listView(ctx, "Datastore", "name", "summary.type", "info")
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	var ds []Datastore
	for _, o := range objs {
		d := Datastore{Name: o.Props["name"].text(), Type: o.Props["summary.type"].text()}
		if info := o.Props["info"]; info != nil {
			d.Host = info.find("remoteHost").text()
			if d.Host == "" {
				d.Host = info.find("remoteHostNames").text()
			}
			d.Path = info.find("remotePath").text()
		}
		names[o.ID] = d.Name
		ds = append(ds, d)
	}
	t.mu.Lock()
	t.dsNames = names
	t.mu.Unlock()
	return ds, nil
}

func (t *soapTransport) VMs(ctx context.Context) ([]VM, error) {
	t.mu.Lock()
	names := t.dsNames
	t.mu.Unlock()
	if names == nil {
		if _, err := t.Datastores(ctx); err != nil {
			return nil, err
		}
		t.mu.Lock()
		names = t.dsNames
		t.mu.Unlock()
	}
	objs, err := t.listView(ctx, "VirtualMachine", "name", "runtime.powerState", "datastore")
	if err != nil {
		return nil, err
	}
	var vms []VM
	for _, o := range objs {
		id, err := strconv.Atoi(o.ID)
		if err != nil {
			t.addWarn("vm " + o.ID + " skipped: only standalone ESXi hosts (numeric VM ids) are supported")
			continue
		}
		vm := VM{ID: id, Name: o.Props["name"].text(), PowerState: o.Props["runtime.powerState"].text()}
		for _, m := range o.Props["datastore"].all("ManagedObjectReference") {
			if n, ok := names[m.Text]; ok {
				vm.Datastores = append(vm.Datastores, n)
			} else {
				vm.Datastores = append(vm.Datastores, m.Text)
			}
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func (t *soapTransport) Snapshots(ctx context.Context, vmid int) ([]Snapshot, error) {
	p, err := t.getProps(ctx, "VirtualMachine", strconv.Itoa(vmid), "snapshot")
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	var walk func(n *xnode)
	walk = func(n *xnode) {
		for _, k := range n.Kids {
			if k.Name == "rootSnapshotList" || k.Name == "childSnapshotList" {
				out = append(out, Snapshot{ID: k.child("snapshot").text(), Name: k.child("name").text(), Desc: k.child("description").text()})
				walk(k)
			}
		}
	}
	if v := p["snapshot"]; v != nil {
		walk(v)
	}
	return out, nil
}

// waitTask polls a vSphere task until it ends and returns its result value.
func (t *soapTransport) waitTask(ctx context.Context, task string) (string, error) {
	for {
		p, err := t.getProps(ctx, "Task", task, "info.state", "info.error", "info.result")
		if err != nil {
			return "", err
		}
		switch p["info.state"].text() {
		case "success":
			return p["info.result"].text(), nil
		case "error":
			if e := p["info.error"]; e != nil {
				if m := e.find("localizedMessage"); m != nil && m.Text != "" {
					return "", errors.New(m.Text)
				}
				if f := e.find("fault"); f != nil {
					return "", errors.New("task failed: " + f.Attr["type"])
				}
			}
			return "", errors.New("task failed")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (t *soapTransport) CreateSnapshot(ctx context.Context, vmid int, name, desc string, mem, quiesce bool) (string, error) {
	r, err := t.call(ctx, fmt.Sprintf(`<CreateSnapshot_Task xmlns="urn:vim25"><_this type="VirtualMachine">%d</_this><name>%s</name><description>%s</description><memory>%t</memory><quiesce>%t</quiesce></CreateSnapshot_Task>`, vmid, xesc(name), xesc(desc), mem, quiesce))
	if err != nil {
		return "", err
	}
	task := r.find("returnval").text()
	if task == "" {
		return "", errors.New("CreateSnapshot_Task returned no task")
	}
	id, err := t.waitTask(ctx, task)
	if err != nil {
		if ctx.Err() != nil {
			t.cancelTask(task) // timed out: do not leave the host busy with a snapshot nobody waits for
		}
		return "", err
	}
	if id == "" {
		return "", errors.New("snapshot created but the API returned no snapshot id")
	}
	return id, nil
}

// cancelTask asks the host to abandon a task the client stopped waiting for and
// waits (a few seconds) until it has. Best effort: not every task is cancelable.
// Without it a timed-out memory snapshot keeps running on the host and the next
// step of the chain fails with "Another task is already in progress".
func (t *soapTransport) cancelTask(task string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := t.call(ctx, `<CancelTask xmlns="urn:vim25"><_this type="Task">`+xesc(task)+`</_this></CancelTask>`); err != nil {
		return
	}
	for {
		p, err := t.getProps(ctx, "Task", task, "info.state")
		if err != nil {
			return
		}
		if st := p["info.state"].text(); st != "running" && st != "queued" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (t *soapTransport) RemoveSnapshot(ctx context.Context, vmid int, snapID string) error {
	if !reMoid.MatchString(snapID) {
		return fmt.Errorf("bad snapshot id %q", snapID)
	}
	r, err := t.call(ctx, `<RemoveSnapshot_Task xmlns="urn:vim25"><_this type="VirtualMachineSnapshot">`+snapID+`</_this><removeChildren>false</removeChildren></RemoveSnapshot_Task>`)
	if err != nil {
		return err
	}
	task := r.find("returnval").text()
	if task == "" {
		return errors.New("RemoveSnapshot_Task returned no task")
	}
	_, err = t.waitTask(ctx, task)
	return err
}
