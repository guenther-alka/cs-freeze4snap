package esxi

import (
	"reflect"
	"testing"
)

// captured from a real ESXi 8.0 host (vim-cmd / esxcli)
const sampleGetAllVMs = `Vmid          Name                                     File                                     Guest OS          Version   Annotation
10     _solaris11.4cbe.50   [192.168.2.203_nvme_nfs] solaris11.4cbe/solaris11.4cbe.vmx   solaris11_64Guest        vmx-19              
25     test.sbb.47          [nfs2] test/test.vmx                                         solaris11_64Guest        vmx-14              
5      dummy delay          [local_datastore] dummy delay/dummy delay.vmx                otherLinuxGuest          vmx-20              
8      omnisan-203          [local_datastore] omnisan/omnisan.vmx                        solaris11_64Guest        vmx-20              
`

const sampleNFSList = `Volume Name             Host           Share          Accessible  Mounted  Read-Only   isPE  Hardware Acceleration
----------------------  -------------  -------------  ----------  -------  ---------  -----  ---------------------
nfs2                    192.168.2.203  /daten1/nfs          true     true      false  false  Not Supported
192.168.2.203_nvme_nfs  192.168.2.203  /nvme/nfs            true     true      false  false  Not Supported
media                   192.168.2.203  /daten1/media        true     true      false  false  Not Supported
`

const sampleSnapTree = `Get Snapshot:
|-ROOT
--Snapshot Name        : test
--Snapshot Id        : 14
--Snapshot Desciption  : 
--Snapshot Created On  : 4/12/2023 16:12:54
--Snapshot State       : powered on
--|-CHILD
----Snapshot Name        : cs4s-x-5
----Snapshot Id        : 15
----Snapshot Desciption  : cs-freeze4snap
----Snapshot Created On  : 5/9/2021 23:50:0
----Snapshot State       : powered off
`

const sampleLoop = `@@25
Powered off
name                 nfs2                           
@@5
Powered on
name                 local_datastore                   192.168.2.203_nvme_nfs            my store                       
@@x
Powered on
`

func TestParseGetAllVMs(t *testing.T) {
	vms := parseGetAllVMs(sampleGetAllVMs)
	if len(vms) != 4 {
		t.Fatalf("want 4 vms, got %d: %+v", len(vms), vms)
	}
	if vms[2].ID != 5 || vms[2].Name != "dummy delay" || vms[2].Datastores[0] != "local_datastore" {
		t.Errorf("vm with blank in name: %+v", vms[2])
	}
	if vms[0].Name != "_solaris11.4cbe.50" || vms[0].Datastores[0] != "192.168.2.203_nvme_nfs" {
		t.Errorf("first vm: %+v", vms[0])
	}
}

func TestParseNFSList(t *testing.T) {
	ds := parseNFSList(sampleNFSList, "NFS")
	want := []Datastore{
		{"nfs2", "NFS", "192.168.2.203", "/daten1/nfs"},
		{"192.168.2.203_nvme_nfs", "NFS", "192.168.2.203", "/nvme/nfs"},
		{"media", "NFS", "192.168.2.203", "/daten1/media"},
	}
	if !reflect.DeepEqual(ds, want) {
		t.Fatalf("got %+v", ds)
	}
	if got := parseNFSList("", "NFS"); got != nil {
		t.Errorf("empty output: %+v", got)
	}
}

func TestParseSnapshotGet(t *testing.T) {
	s := parseSnapshotGet(sampleSnapTree)
	want := []Snapshot{{"14", "test", ""}, {"15", "cs4s-x-5", "cs-freeze4snap"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v", s)
	}
	if got := parseSnapshotGet("Get Snapshot:\n"); len(got) != 0 {
		t.Errorf("no snapshots expected: %+v", got)
	}
}

func TestParseLoop(t *testing.T) {
	m := parseLoop(sampleLoop)
	if m[25].PowerState != PowerOff || !reflect.DeepEqual(m[25].Datastores, []string{"nfs2"}) {
		t.Errorf("vm 25: %+v", m[25])
	}
	if m[5].PowerState != PowerOn || !reflect.DeepEqual(m[5].Datastores, []string{"local_datastore", "192.168.2.203_nvme_nfs", "my store"}) {
		t.Errorf("vm 5: %+v", m[5])
	}
}

func TestParsePowerState(t *testing.T) {
	for in, want := range map[string]string{"Powered on": PowerOn, "Powered off\n": PowerOff, "Suspended": PowerSuspended, "Retrieved runtime info\nPowered off": PowerOff, "?": ""} {
		if got := parsePowerState(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestVimFault(t *testing.T) {
	if vimFault("Create Snapshot:\n") {
		t.Error("clean output flagged")
	}
	if !vimFault("(vim.fault.ToolsUnavailable) {\n  msg = \"x\"\n}") {
		t.Error("fault not detected")
	}
}
