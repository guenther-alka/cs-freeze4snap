package esxi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tableFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "freeze4snap.cfg")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleTable = "\xef\xbb\xbf# host,user,password\r\n" +
	"192.168.2.48,root,123\r\n" +
	"\n" +
	"esx2.lan, admin ,pa,ss word ,\r\n" + // password holds a comma; only the edges are trimmed
	"192.168.2.50,cert\n" +
	"192.168.2.51,ops,cert\n" +
	"192.168.2.52,root,cert,/etc/keys/esx52\n"

func TestHostTableLookup(t *testing.T) {
	p := tableFile(t, sampleTable)
	cases := []struct {
		host, user, pw, key string
	}{
		{"192.168.2.48", "root", "123", ""},
		{"ESX2.LAN", "admin", "pa,ss word ,", ""}, // case-insensitive host, comma kept
		{"192.168.2.51", "ops", "", "KEY"},
		{"192.168.2.52", "root", "", "/etc/keys/esx52"},
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("x"), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, c := range cases {
		got, err := LoadConfigFor(p, c.host)
		if err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		wantKey := c.key
		if wantKey == "KEY" {
			wantKey = filepath.Join(home, ".ssh", "id_ed25519")
		}
		if got.User != c.user || got.Password != c.pw || got.Key != wantKey {
			t.Errorf("%s: got %+v", c.host, got)
		}
	}
	if got, err := LoadConfigFor(p, "192.168.2.50"); err != nil || got.User != "root" || got.Key == "" || got.Password != "" {
		t.Errorf("host,cert: %+v %v", got, err)
	}
}

func TestHostTableErrors(t *testing.T) {
	for name, tc := range map[string]struct{ body, host, want string }{
		"unknown host":     {"a,root,1\nb,root,2\n", "c", "no entry for \"c\" (has: a, b)"},
		"needs --host":     {"a,root,1\nb,root,2\n", "", "name one with --host"},
		"user w/o pw":      {"a,root\n", "a", "user but no password"},
		"duplicate":        {"a,root,1\nA,root,2\n", "a", "listed twice"},
		"empty password":   {"a,root,\n", "a", "empty password"},
		"no key available": {"a,cert\n", "a", "no ssh key found"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			_, err := LoadConfigFor(tableFile(t, tc.body), tc.host)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHostTableSingleEntryNeedsNoHost(t *testing.T) {
	c, err := LoadConfigFor(tableFile(t, "only.host,root,pw\n"), "")
	if err != nil || c.Host != "only.host" || c.Password != "pw" {
		t.Errorf("%+v %v", c, err)
	}
}

func TestFormatDetection(t *testing.T) {
	for in, want := range map[string]bool{
		"h,root,pw":                 true,
		"# c\n\nh,cert":             true,
		"host=1.2.3.4":              false,
		"password=a,b\nhost=x":      false, // comma inside a key=value password
		"":                          false,
		"# only comments":           false,
		"\xef\xbb\xbfh,root,secret": true,
	} {
		if got := isHostTable(in); got != want {
			t.Errorf("isHostTable(%q) = %v, want %v", in, got, want)
		}
	}
	// key=value files still go through LoadConfig
	p := tableFile(t, "host=10.1.1.1\npassword=a,b\n")
	c, err := LoadConfigFor(p, "")
	if err != nil || c.Host != "10.1.1.1" || c.Password != "a,b" {
		t.Errorf("key=value via LoadConfigFor: %+v %v", c, err)
	}
	// a table entry without password cannot use soap
	c = Config{Host: "h", Key: "/k", Proto: "soap"}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "ssh only") {
		t.Errorf("soap + cert must say so, got %v", err)
	}
}
