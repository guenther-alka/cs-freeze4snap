package esxi

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
)

// xnode is a minimal XML tree (local names only, namespaces ignored) - enough
// to read vSphere SOAP answers without a code generator.
type xnode struct {
	Name string
	Attr map[string]string
	Text string
	Kids []*xnode
}

func parseXML(b []byte) (*xnode, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var stack []*xnode
	var root *xnode
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{Name: t.Name.Local, Attr: map[string]string{}}
			for _, a := range t.Attr {
				n.Attr[a.Name.Local] = a.Value
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.Kids = append(p.Kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				n := stack[len(stack)-1]
				n.Text = strings.TrimSpace(n.Text)
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return root, nil
}

// find returns the first descendant (depth first, including n) named name.
func (n *xnode) find(name string) *xnode {
	if n == nil {
		return nil
	}
	if n.Name == name {
		return n
	}
	for _, k := range n.Kids {
		if r := k.find(name); r != nil {
			return r
		}
	}
	return nil
}

// all returns every descendant named name (not descending into matches).
func (n *xnode) all(name string) []*xnode {
	var out []*xnode
	if n == nil {
		return nil
	}
	if n.Name == name {
		return []*xnode{n}
	}
	for _, k := range n.Kids {
		out = append(out, k.all(name)...)
	}
	return out
}

// child returns the first direct child named name.
func (n *xnode) child(name string) *xnode {
	if n == nil {
		return nil
	}
	for _, k := range n.Kids {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func (n *xnode) text() string {
	if n == nil {
		return ""
	}
	return n.Text
}

func xesc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
