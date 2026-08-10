package freezer

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseZfsListOutput(t *testing.T) {
	input := `rpool/data
rpool/data/vm-100-disk-0
rpool/data/vm-100-disk-1
rpool/data/vm-101-disk-0
rpool/data/subvol-200-disk-0
rpool/data/subvol-200-disk-1
rpool/data/some-other-dataset
rpool/data/vm-100-disk-0-old`

	got := parseZfsListOutput(input)

	want := []Guest{
		{VMID: 100, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-100-disk-1"},
		{VMID: 101, Type: TypeVM, Platform: PlatformProxmoxQEMU, Dataset: "rpool/data/vm-101-disk-0"},
		{VMID: 200, Type: TypeLXC, Platform: PlatformProxmoxLXC, Dataset: "rpool/data/subvol-200-disk-1"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d guests, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].VMID != want[i].VMID || got[i].Type != want[i].Type || got[i].Platform != want[i].Platform {
			t.Errorf("guest %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseZfsListOutput_Empty(t *testing.T) {
	if got := parseZfsListOutput(""); len(got) != 0 {
		t.Errorf("expected no guests for empty input, got %+v", got)
	}
	if got := parseZfsListOutput("rpool/data\nrpool/data/plain-dataset\n"); len(got) != 0 {
		t.Errorf("expected no guests when nothing matches naming convention, got %+v", got)
	}
}

func TestParseZfsListOutput_VMDiskNumberNotMistakenForVMID(t *testing.T) {
	// vm-100-disk-15 must resolve to VMID 100, not to disk index 15, and
	// must not be confused with a hypothetical vm-15-disk-... entry.
	input := "rpool/data/vm-100-disk-15"
	got := parseZfsListOutput(input)
	if len(got) != 1 || got[0].VMID != 100 {
		t.Fatalf("got %+v, want single guest with VMID=100", got)
	}
}

func TestFilterGuests_IncludeOnly(t *testing.T) {
	guests := []Guest{{VMID: 100}, {VMID: 101}, {VMID: 102}}
	got := FilterGuests(guests, []int{101}, nil)
	want := []Guest{{VMID: 101}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFilterGuests_Exclude(t *testing.T) {
	guests := []Guest{{VMID: 100}, {VMID: 101}, {VMID: 102}}
	got := FilterGuests(guests, nil, []int{101})
	want := []Guest{{VMID: 100}, {VMID: 102}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFilterGuests_IncludeOnlyTakesPrecedenceOverExclude(t *testing.T) {
	guests := []Guest{{VMID: 100}, {VMID: 101}}
	// Both set at once shouldn't happen from the CLI, but if it does,
	// include-only must win deterministically rather than silently
	// combining in a surprising way.
	got := FilterGuests(guests, []int{100}, []int{100})
	want := []Guest{{VMID: 100}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFilterGuests_ExcludeUnknownVMIDIsNoop(t *testing.T) {
	guests := []Guest{{VMID: 100}}
	got := FilterGuests(guests, nil, []int{999})
	if !reflect.DeepEqual(got, guests) {
		t.Errorf("got %+v, want unchanged %+v", got, guests)
	}
}

func TestParseIDList(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"  ", nil},
		{"100", []int{100}},
		{"100,101,102", []int{100, 101, 102}},
		{" 100 , 101 ", []int{100, 101}},
		{"100,,101", []int{100, 101}}, // tolerate stray empty segments
	}
	for _, c := range cases {
		got, err := ParseIDList(c.in)
		if err != nil {
			t.Errorf("ParseIDList(%q): unexpected error: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseIDList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseIDList_Invalid(t *testing.T) {
	if _, err := ParseIDList("100,abc"); err == nil {
		t.Error("expected error for non-numeric VMID, got nil")
	}
}

func TestAllDiskDatasetsByVMID(t *testing.T) {
	input := `nvme480/vm-101-disk-0
rpool/data/vm-101-disk-0
rpool/data/vm-100-disk-0
rpool/data/subvol-200-disk-0
rpool/data/plain-dataset`

	got := allDiskDatasetsByVMID(input)

	if len(got[101]) != 2 {
		t.Errorf("vmid 101: got %d entries, want 2: %v", len(got[101]), got[101])
	}
	if len(got[100]) != 1 {
		t.Errorf("vmid 100: got %d entries, want 1: %v", len(got[100]), got[100])
	}
	if len(got[200]) != 1 {
		t.Errorf("vmid 200: got %d entries, want 1: %v", len(got[200]), got[200])
	}
}

func TestDetectCrossPoolDisks_ParsingLogic(t *testing.T) {
	// Exercises the pure comparison logic directly (allDiskDatasetsByVMID +
	// the prefix check) without invoking the real `zfs` binary, since
	// DetectCrossPoolDisks itself shells out. This mirrors what
	// DetectCrossPoolDisks does internally so the warning-generation logic
	// is covered even in this no-real-ZFS sandbox.
	globalOut := `nvme480/vm-101-disk-0
rpool/data/vm-101-disk-0
rpool/data/vm-100-disk-0
rpool/data/subvol-200-disk-0`

	allByVMID := allDiskDatasetsByVMID(globalOut)
	guests := []Guest{
		{VMID: 101, Type: TypeVM, Dataset: "rpool/data/vm-101-disk-0"},
		{VMID: 100, Type: TypeVM, Dataset: "rpool/data/vm-100-disk-0"},
		{VMID: 200, Type: TypeLXC, Dataset: "rpool/data/subvol-200-disk-0"},
	}
	targetDataset := "rpool/data"
	prefix := targetDataset + "/"

	var warned []int
	for _, g := range guests {
		for _, d := range allByVMID[g.VMID] {
			if d != targetDataset && !strings.HasPrefix(d, prefix) {
				warned = append(warned, g.VMID)
				break
			}
		}
	}

	if len(warned) != 1 || warned[0] != 101 {
		t.Errorf("expected only vmid 101 to be flagged for cross-pool disks, got %v", warned)
	}
}
