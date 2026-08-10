package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

// fake_qga simulates a QEMU Guest Agent unix socket for one VM, so
// cs-freeze4snap's real QGA client code path can be exercised end-to-end
// without a real QEMU/KVM VM. Usage: fake_qga <socket-path>
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fake_qga <socket-path>")
		os.Exit(2)
	}
	sockPath := os.Args[1]
	os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	defer l.Close()
	fmt.Println("fake_qga listening on", sockPath)

	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fmt.Fprintln(os.Stderr, "fake_qga received:", line)

		var resp string
		switch {
		case contains(line, "guest-ping"):
			resp = `{"return":{}}`
		case contains(line, "guest-fsfreeze-freeze"):
			resp = `{"return":1}`
		case contains(line, "guest-fsfreeze-thaw"):
			resp = `{"return":1}`
		case contains(line, "qmp_capabilities"):
			resp = `{"return":{}}`
		case contains(line, `"stop"`):
			resp = `{"return":{}}`
		case contains(line, `"cont"`):
			resp = `{"return":{}}`
		default:
			resp = `{"error":{"class":"CommandNotFound","desc":"unhandled by fake_qga"}}`
		}
		conn.Write([]byte(resp + "\n"))
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
