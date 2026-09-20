package esxi

import (
	"fmt"
	"strings"
)

// warnTransport adds warnings to a transport.
type warnTransport struct {
	Transport
	extra []string
}

func (w *warnTransport) Warnings() []string {
	return append(append([]string{}, w.extra...), w.Transport.Warnings()...)
}

// openAuto is proto "auto" (the default): the vSphere API (soap) first, ssh if
// that cannot be reached. A failed soap *login* is final - ssh would use the
// same credentials, and every failed login counts towards the account lockout
// of the ESXi host.
func openAuto(c Config) (Transport, error) { return openAutoWith(c, Open) }

func openAutoWith(c Config, open func(Config) (Transport, error)) (Transport, error) {
	sc, hc := c, c
	sc.Proto, hc.Proto = "soap", "ssh"
	sc.Port, hc.Port = 0, 0 // an explicit port needs an explicit proto
	if sc.Password == "" {  // a key entry works with ssh only
		return open(hc)
	}
	tr, serr := open(sc)
	if serr == nil {
		return tr, nil
	}
	if strings.Contains(serr.Error(), "soap login as") {
		return nil, serr
	}
	tr, herr := open(hc)
	if herr != nil {
		return nil, fmt.Errorf("soap: %v; ssh: %v", serr, herr)
	}
	return &warnTransport{Transport: tr, extra: []string{"soap not usable (" + serr.Error() + ") - using ssh"}}, nil
}
