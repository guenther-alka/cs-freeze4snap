package esxi

import (
	"context"
	"os"
	"strings"
	"testing"
)

// fakeTransport serves canned datastores and VMs.
type fakeTransport struct {
	ds  []Datastore
	vms []VM
}

func (f *fakeTransport) Name() string                                       { return "fake" }
func (f *fakeTransport) Datastores(context.Context) ([]Datastore, error)    { return f.ds, nil }
func (f *fakeTransport) VMs(context.Context) ([]VM, error)                  { return f.vms, nil }
func (f *fakeTransport) Snapshots(context.Context, int) ([]Snapshot, error) { return nil, nil }
func (f *fakeTransport) Warnings() []string                                 { return nil }
func (f *fakeTransport) Close() error                                       { return nil }
func (f *fakeTransport) RemoveSnapshot(context.Context, int, string) error  { return nil }
func (f *fakeTransport) CreateSnapshot(context.Context, int, string, string, bool, bool) (string, error) {
	return "1", nil
}

func lab() *fakeTransport {
	return &fakeTransport{
		ds: []Datastore{
			{"nfs2", "NFS", "192.168.2.203", "/daten1/nfs"},
			{"nvme", "NFS", "192.168.2.203", "/nvme/nfs"},
			{"nvme-sub", "NFS", "192.168.2.203", "/nvme/nfs/sub"},
			{"local", "VMFS", "", ""},
		},
		vms: []VM{
			{1, "a", PowerOn, []string{"nvme"}},
			{2, "b", PowerOff, []string{"nvme"}},
			{3, "c", PowerOn, []string{"nvme", "local"}},
			{4, "d", PowerOn, []string{"local"}},
			{5, "e", PowerOn, []string{"nfs2"}},
			{6, "f", PowerSuspended, []string{"nvme"}},
			{7, "g", PowerOn, []string{"nvme-sub"}},
		},
	}
}

func ids(vms []VM) string {
	var s []string
	for _, v := range vms {
		s = append(s, v.Name)
	}
	return strings.Join(s, ",")
}

func TestDiscoverNFS(t *testing.T) {
	d, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nvme/nfs/"})
	if err != nil {
		t.Fatal(err)
	}
	if ids(d.VMs) != "a" {
		t.Errorf("vms: %s", ids(d.VMs))
	}
	reasons := map[string]string{}
	for _, s := range d.Skipped {
		reasons[s.Name] = s.Reason
	}
	if !strings.Contains(reasons["b"], "powered off") || !strings.Contains(reasons["f"], "suspended") || !strings.Contains(reasons["c"], "local") {
		t.Errorf("skipped: %+v", d.Skipped)
	}
	if _, ok := reasons["d"]; ok || len(d.Datastores) != 1 {
		t.Errorf("vm d is not on the nfs, datastores %+v", d.Datastores)
	}
}

func TestDiscoverNFSRecursiveAndMixed(t *testing.T) {
	d, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nvme/nfs", Recursive: true, AllowMixed: true})
	if err != nil {
		t.Fatal(err)
	}
	if ids(d.VMs) != "a,c,g" || len(d.Warnings) != 1 || len(d.Datastores) != 2 {
		t.Errorf("vms %s warnings %v datastores %d", ids(d.VMs), d.Warnings, len(d.Datastores))
	}
}

func TestDiscoverNFSFilter(t *testing.T) {
	d, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nvme/nfs", Recursive: true, VMs: "g, 1"})
	if err != nil {
		t.Fatal(err)
	}
	if ids(d.VMs) != "a,g" {
		t.Errorf("vms: %s", ids(d.VMs))
	}
}

func TestDiscoverNFSErrors(t *testing.T) {
	if _, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nope"}); err == nil || !strings.Contains(err.Error(), "no NFS datastore") {
		t.Errorf("want no-datastore error, got %v", err)
	}
	f := lab()
	f.ds = append(f.ds, Datastore{"other", "NFS", "10.0.0.9", "/nvme/nfs"})
	if _, err := DiscoverNFS(context.Background(), f, DiscoverOpts{Path: "/nvme/nfs"}); err == nil || !strings.Contains(err.Error(), "--nfs-server") {
		t.Errorf("want ambiguity error, got %v", err)
	}
	d, err := DiscoverNFS(context.Background(), f, DiscoverOpts{Path: "/nvme/nfs", Server: "192.168.2.203"})
	if err != nil || len(d.Datastores) != 1 {
		t.Errorf("server filter: %v %+v", err, d)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func TestDiscoverIncludeOff(t *testing.T) {
	d, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nvme/nfs", IncludeOff: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(d.VMs); got != "a,b,f" {
		t.Errorf("vms = %s, want a,b,f (on, off, suspended); skipped %+v", got, d.Skipped)
	}
}

func TestDiscoverOnNFSIgnoresFilters(t *testing.T) {
	d, err := DiscoverNFS(context.Background(), lab(), DiscoverOpts{Path: "/nvme/nfs", VMs: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(d.OnNFS); got != "a,b,c,f" {
		t.Errorf("OnNFS = %s, want every vm with files on the export: a,b,c,f", got)
	}
}
