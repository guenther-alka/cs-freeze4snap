package esxi

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The cfg file may also hold freeze policy lines (chains, see the freezer
// package) and per-host connection options; the connection parsers skip the
// first and read the second:
//
//	[quiesce,memory,zfs,30]              global chain
//	192.168.2.48:vm100,memory,zfs        chain of one VM
//	192.168.2.48:proto=ssh               connection option of one host
//	chain=memory,zfs / vm100=quiesce     the same in the key=value form

// isPolicyLine reports whether a cfg line is a chain line or a "host:option" line.
func isPolicyLine(t string) bool {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "[") {
		return true
	}
	first := t
	if i := strings.IndexAny(t, ",="); i >= 0 {
		first = t[:i]
	}
	if strings.Contains(first, ":") {
		return true
	}
	if i := strings.Index(t, "="); i > 0 {
		k := strings.ToLower(strings.TrimSpace(t[:i]))
		if k == "chain" {
			return true
		}
		if strings.HasPrefix(k, "vm") || strings.HasPrefix(k, "ct") {
			if n, err := strconv.Atoi(k[2:]); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}

// applyHostOptions reads "host:key=value" lines of the table form (proto, port,
// hostkey, tls_sha256, timeout, useragent) for the given host.
func applyHostOptions(path, text, host string, c *Config) error {
	for n, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(l, string([]byte{0xEF, 0xBB, 0xBF})))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		i := strings.Index(t, ":")
		if i <= 0 || !strings.EqualFold(strings.TrimSpace(t[:i]), host) {
			continue
		}
		rest := stripTrailingComment(t[i+1:])
		eq := strings.Index(rest, "=")
		if eq < 0 {
			continue // a chain line
		}
		k := strings.ToLower(strings.TrimSpace(rest[:eq]))
		v := strings.TrimSpace(rest[eq+1:])
		switch k {
		case "proto":
			c.Proto = strings.ToLower(v)
		case "port":
			p, err := strconv.Atoi(v)
			if err != nil || p <= 0 {
				return fmt.Errorf("cfg %s line %d: bad port %q", path, n+1, v)
			}
			c.Port = p
		case "hostkey":
			c.HostKey = v
		case "tls_sha256":
			c.TLSSHA256 = v
		case "useragent":
			c.UserAgent = v
		case "timeout":
			s, err := strconv.Atoi(v)
			if err != nil || s <= 0 {
				return fmt.Errorf("cfg %s line %d: bad timeout %q", path, n+1, v)
			}
			c.Timeout = time.Duration(s) * time.Second
		}
	}
	return nil
}

func stripTrailingComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
