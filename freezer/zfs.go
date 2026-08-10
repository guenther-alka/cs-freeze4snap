package freezer

import (
	"fmt"
	"os/exec"
	"strings"
)

// Snapshot creates a ZFS snapshot at dataset@name. If recursive is true,
// -r is passed so all child datasets under dataset get the same snapshot
// name atomically (relevant when a guest's disks/volumes span more than
// one dataset under a shared parent).
func Snapshot(dataset, name string, recursive bool) error {
	target := fmt.Sprintf("%s@%s", dataset, name)
	args := []string{"snapshot"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, target)

	cmd := exec.Command("zfs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
