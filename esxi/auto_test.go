package esxi

import (
	"errors"
	"strings"
	"testing"
)

type nameTr struct {
	Transport
	name string
}

func (n nameTr) Name() string       { return n.name }
func (n nameTr) Warnings() []string { return nil }

func TestOpenAuto(t *testing.T) {
	var tried []string
	mk := func(soapErr, sshErr error) func(Config) (Transport, error) {
		return func(c Config) (Transport, error) {
			tried = append(tried, c.Proto)
			if c.Proto == "soap" {
				if soapErr != nil {
					return nil, soapErr
				}
				return nameTr{name: "soap"}, nil
			}
			if sshErr != nil {
				return nil, sshErr
			}
			return nameTr{name: "ssh"}, nil
		}
	}
	c := Config{Host: "h", User: "root", Password: "pw"}

	tried = nil
	tr, err := openAutoWith(c, mk(nil, nil))
	if err != nil || tr.Name() != "soap" || len(tried) != 1 {
		t.Errorf("soap first: %v %v %v", tr, err, tried)
	}

	tried = nil
	tr, err = openAutoWith(c, mk(errors.New("dial tcp: connection refused"), nil))
	if err != nil || tr.Name() != "ssh" || len(tr.Warnings()) != 1 || !strings.Contains(tr.Warnings()[0], "using ssh") {
		t.Errorf("fallback: %v %v", tr, err)
	}

	tried = nil // a wrong password must not be tried twice (account lockout)
	_, err = openAutoWith(c, mk(errors.New("soap login as root: InvalidLogin"), nil))
	if err == nil || len(tried) != 1 {
		t.Errorf("login failure: %v %v", err, tried)
	}

	tried = nil // key entry: ssh only
	_, err = openAutoWith(Config{Host: "h", Key: "/k"}, mk(nil, nil))
	if err != nil || len(tried) != 1 || tried[0] != "ssh" {
		t.Errorf("key: %v %v", err, tried)
	}

	_, err = openAutoWith(c, mk(errors.New("a"), errors.New("b")))
	if err == nil || !strings.Contains(err.Error(), "soap: a; ssh: b") {
		t.Errorf("both fail: %v", err)
	}
}
