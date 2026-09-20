package esxi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The host table is the multi-server form of the cfg file, one server per line:
//
//	# host,user,password
//	192.168.2.48,root,secret
//	192.168.2.49,root,pass,word with commas   (the password is everything after the 2nd comma)
//	192.168.2.50,cert                          (ssh key of the running user, user root)
//	192.168.2.51,admin,cert                    (ssh key of the running user)
//	192.168.2.52,root,cert,/etc/keys/esxi52    (explicit OpenSSH private key)
//
// The line of the host asked for is used; per-connection options (port, hostkey,
// tls_sha256, ...) are not part of the table - use a key=value file for those.

type hostEntry struct {
	Host, User, Password, Key string
}

// isHostTable reports whether the first entry line looks like "host,..." rather
// than "key=value": whatever of "=" and "," comes first decides (a host name
// never holds an "=", a key=value password may hold a comma).
func isHostTable(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(l, string([]byte{0xEF, 0xBB, 0xBF})))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		c, e := strings.Index(t, ","), strings.Index(t, "=")
		return c >= 0 && (e < 0 || c < e)
	}
	return false
}

func parseHostTable(path, text string) ([]hostEntry, error) {
	var out []hostEntry
	seen := map[string]bool{}
	for n, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(l, string([]byte{0xEF, 0xBB, 0xBF})))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Split(t, ",")
		for i := range f[:2] {
			f[i] = strings.TrimSpace(f[i])
		}
		e := hostEntry{Host: f[0]}
		switch {
		case e.Host == "" || len(f) < 2 || f[1] == "":
			return nil, fmt.Errorf("cfg %s line %d: expected host,user,password or host,cert", path, n+1)
		case len(f) == 2 && strings.EqualFold(f[1], "cert"):
			e.User, e.Key = "root", "default"
		case len(f) == 2:
			return nil, fmt.Errorf("cfg %s line %d: %q has a user but no password (host,user,password) - for a key use host,cert", path, n+1, e.Host)
		default:
			e.User = f[1]
			if v := strings.TrimSpace(f[2]); strings.EqualFold(v, "cert") {
				e.Key = "default"
				if len(f) > 3 {
					if k := strings.TrimSpace(strings.Join(f[3:], ",")); k != "" {
						e.Key = k
					}
				}
			} else {
				e.Password = strings.TrimRight(strings.Join(f[2:], ","), " \r\t")
				if e.Password == "" {
					return nil, fmt.Errorf("cfg %s line %d: empty password for %q", path, n+1, e.Host)
				}
			}
		}
		k := strings.ToLower(e.Host)
		if seen[k] {
			return nil, fmt.Errorf("cfg %s line %d: host %q is listed twice", path, n+1, e.Host)
		}
		seen[k] = true
		out = append(out, e)
	}
	return out, nil
}

// defaultSSHKey finds the private key of the running user.
func defaultSSHKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cert login: no home directory: %w", err)
	}
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", n)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("cert login: no ssh key found in %s (id_ed25519, id_ecdsa, id_rsa) - name one as host,user,cert,/path/key", filepath.Join(home, ".ssh"))
}

// LoadConfigFor reads a cfg file in either form: the key=value file of a single
// server (host may be "" then), or the host table, from which the line of host
// is taken. With a table and an empty host the file must hold exactly one server.
func LoadConfigFor(path, host string) (Config, error) {
	if path == "" {
		return Config{Host: host}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("cfg: %w", err)
	}
	if !isHostTable(string(b)) {
		return LoadConfig(path)
	}
	es, err := parseHostTable(path, string(b))
	if err != nil {
		return Config{}, err
	}
	var e *hostEntry
	switch {
	case host == "" && len(es) == 1:
		e = &es[0]
	case host == "":
		return Config{}, fmt.Errorf("cfg %s lists %d servers (%s) - name one with --host", path, len(es), hostNames(es))
	default:
		for i := range es {
			if strings.EqualFold(es[i].Host, host) {
				e = &es[i]
			}
		}
		if e == nil {
			return Config{}, fmt.Errorf("cfg %s has no entry for %q (has: %s)", path, host, hostNames(es))
		}
	}
	c := Config{Host: e.Host, User: e.User, Password: e.Password}
	if e.Key != "" {
		k := e.Key
		if k == "default" {
			if k, err = defaultSSHKey(); err != nil {
				return Config{}, err
			}
		}
		c.Key = k
	}
	return c, nil
}

func hostNames(es []hostEntry) string {
	var n []string
	for _, e := range es {
		n = append(n, e.Host)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}
