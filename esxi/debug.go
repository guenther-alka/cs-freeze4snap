package esxi

import (
	"fmt"
	"os"
)

// CS_ESXI_DEBUG=1 prints every ssh command / soap call with its duration to stderr.
var debugOn = os.Getenv("CS_ESXI_DEBUG") != ""

func debugf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[esxi] "+format+"\n", a...)
}
