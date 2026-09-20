package esxi

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config describes how to reach one ESXi host. It is read from a simple
// key=value file (see LoadConfig); environment variables and command line
// flags may override single values, the password is never a flag.
type Config struct {
	Host      string        // ip or name of the ESXi host
	Port      int           // default 22 (ssh) / 443 (soap)
	Proto     string        // ssh (default) or soap
	User      string        // default root
	Password  string        // password (ssh password / keyboard-interactive, soap login)
	Key       string        // ssh only: path to an OpenSSH private key (PuTTY .ppk is not supported)
	KeyPass   string        // ssh only: passphrase of that key
	HostKey   string        // ssh only: pinned host key fingerprint "SHA256:..." (unset: accepted with a warning)
	TLSSHA256 string        // soap only: pinned certificate fingerprint, sha256 hex (unset: not verified, warning)
	UserAgent string        // soap only: overrides the default VMware client User-Agent
	Timeout   time.Duration // connect timeout, default 15s
}

// LoadConfig reads key=value lines ("#" starts a comment, values are taken
// literally after the first "="). Known keys: host, port, proto, user,
// password, key, key_passphrase, hostkey, tls_sha256, useragent, timeout
// (seconds). An empty path returns an empty Config.
func LoadConfig(path string) (Config, error) {
	var c Config
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("cfg: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string([]byte{0xEF, 0xBB, 0xBF})))
		if t == "" || strings.HasPrefix(t, "#") || isPolicyLine(t) {
			continue
		}
		i := strings.Index(t, "=")
		if i < 0 {
			return c, fmt.Errorf("cfg %s line %d: expected key=value", path, line)
		}
		k := strings.ToLower(strings.TrimSpace(t[:i]))
		v := strings.TrimSpace(t[i+1:])
		switch k {
		case "host":
			c.Host = v
		case "port":
			n, err := strconv.Atoi(v)
			if err != nil {
				return c, fmt.Errorf("cfg %s line %d: bad port %q", path, line, v)
			}
			c.Port = n
		case "proto":
			c.Proto = strings.ToLower(v)
		case "user":
			c.User = v
		case "password":
			c.Password = v
		case "key":
			c.Key = v
		case "key_passphrase":
			c.KeyPass = v
		case "hostkey":
			c.HostKey = v
		case "tls_sha256":
			c.TLSSHA256 = v
		case "useragent":
			c.UserAgent = v
		case "timeout":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("cfg %s line %d: bad timeout %q", path, line, v)
			}
			c.Timeout = time.Duration(n) * time.Second
		default:
			return c, fmt.Errorf("cfg %s line %d: unknown key %q", path, line, k)
		}
	}
	return c, sc.Err()
}

// ApplyEnv lets CS_ESXI_HOST / CS_ESXI_USER / CS_ESXI_PASSWORD override the file.
func (c *Config) ApplyEnv() {
	if v := os.Getenv("CS_ESXI_HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("CS_ESXI_USER"); v != "" {
		c.User = v
	}
	if v := os.Getenv("CS_ESXI_PASSWORD"); v != "" {
		c.Password = v
	}
}

// Normalize fills defaults and validates the result.
func (c *Config) Normalize() error {
	if c.Proto == "" || c.Proto == "auto" {
		c.Proto = "ssh"
	}
	if c.Proto != "ssh" && c.Proto != "soap" {
		return fmt.Errorf("proto must be ssh or soap, not %q", c.Proto)
	}
	if c.User == "" {
		c.User = "root"
	}
	if c.Port == 0 {
		if c.Proto == "ssh" {
			c.Port = 22
		} else {
			c.Port = 443
		}
	}
	if c.Timeout == 0 {
		c.Timeout = 15 * time.Second
	}
	if c.Host == "" {
		return fmt.Errorf("no ESXi host: set host= in the cfg file or CS_ESXI_HOST")
	}
	if c.Password == "" && c.Key == "" {
		return fmt.Errorf("no credentials: set password= (or key= for ssh) in the cfg file, or CS_ESXI_PASSWORD")
	}
	if c.Proto == "soap" && c.Password == "" {
		return fmt.Errorf("proto soap needs a password (a cert/key entry works with ssh only)")
	}
	return nil
}

// checkAuto validates what proto auto needs before trying anything.
func checkAuto(c Config) error {
	if c.Host == "" {
		return fmt.Errorf("no ESXi host: set host= in the cfg file or CS_ESXI_HOST")
	}
	if c.Password == "" && c.Key == "" {
		return fmt.Errorf("no credentials: set password= (or key= for ssh) in the cfg file, or CS_ESXI_PASSWORD")
	}
	return nil
}

// Open connects with the transport selected by c.Proto (ssh, soap; "" or auto:
// soap first, ssh as fallback).
func Open(c Config) (Transport, error) {
	if c.Proto == "" || c.Proto == "auto" {
		if err := checkAuto(c); err != nil {
			return nil, err
		}
		return openAuto(c)
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	// Return an untyped nil on error: a nil *transport inside the interface
	// would look like a live connection to callers.
	if c.Proto == "soap" {
		t, err := openSOAP(c)
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	t, err := openSSH(c)
	if err != nil {
		return nil, err
	}
	return t, nil
}
