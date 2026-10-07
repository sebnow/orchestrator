// Package html builds HTML as a tree of nodes and writes it out. Text and
// attribute values are always escaped; Raw is the one way to write markup
// verbatim.
package html

import (
	"fmt"
	"html"
	"io"
	"strings"
)

// Node is a piece of HTML. Only this package makes Nodes, so every string
// in a document went through Text, Attr or Raw.
type Node interface{ writeTo(w *writer) }

// writer keeps the first write error, so that nodes need not check each.
type writer struct {
	out io.Writer
	err error
}

func (w *writer) write(s string) {
	if w.err == nil {
		_, w.err = io.WriteString(w.out, s)
	}
}

// Render writes node and everything under it to w.
func Render(w io.Writer, node Node) error {
	out := &writer{out: w}
	node.writeTo(out)
	return out.err
}

type text string
type raw string
type fragment []Node

func (t text) writeTo(w *writer) { w.write(html.EscapeString(string(t))) }
func (r raw) writeTo(w *writer)  { w.write(string(r)) }
func (f fragment) writeTo(w *writer) {
	for _, child := range f {
		if child != nil {
			child.writeTo(w)
		}
	}
}

// Text is s as text content, escaped.
func Text(s string) Node { return text(s) }

// Raw is s written verbatim. Every caller must say why s is safe.
func Raw(s string) Node { return raw(s) }

// Fragment is children side by side in no element. Nil children, here
// and in El, are skipped.
func Fragment(children ...Node) Node { return fragment(children) }

// Attribute is one attribute of an element. The zero Attribute writes
// nothing, so that an attribute can be left out of a list.
type Attribute struct{ name, value string }

func Attr(name, value string) Attribute { return Attribute{name: requireName(name), value: value} }

const voidElements = " area base br col embed hr img input link meta source track wbr "

type element struct {
	tag      string
	void     bool
	attrs    []Attribute
	children fragment
}

// El is the element tag with attrs and children. A void element takes no
// children and has no end tag; every other element is closed.
func El(tag string, attrs []Attribute, children ...Node) Node {
	if strings.Contains(voidElements, " "+requireName(tag)+" ") {
		if len(children) > 0 {
			panic(fmt.Sprintf("html: void element <%s> given children", tag))
		}
		return element{tag: tag, void: true, attrs: attrs}
	}
	return element{tag: tag, attrs: attrs, children: children}
}

func (e element) writeTo(w *writer) {
	w.write("<" + e.tag)
	for _, attr := range e.attrs {
		if attr.name != "" {
			w.write(" " + attr.name + `="` + html.EscapeString(attr.value) + `"`)
		}
	}
	w.write(">")
	if !e.void {
		e.children.writeTo(w)
		w.write("</" + e.tag + ">")
	}
}

// requireName panics unless name is a lowercase tag or attribute name.
// Names come from code, never from data, so a bad one is a bug.
func requireName(name string) string {
	for idx, r := range name {
		if !(r >= 'a' && r <= 'z' || idx > 0 && (r >= '0' && r <= '9' || r == '-')) {
			panic(fmt.Sprintf("html: invalid name %q", name))
		}
	}
	if name == "" {
		panic("html: empty name")
	}
	return name
}
