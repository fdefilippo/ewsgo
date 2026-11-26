// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ews implements a deliberately bounded subset of EWS from its schema.
package ews

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
)

const soap = "http://schemas.xmlsoap.org/soap/envelope/"
const messages = "http://schemas.microsoft.com/exchange/services/2006/messages"
const types = "http://schemas.microsoft.com/exchange/services/2006/types"

type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []node     `xml:",any"`
}

func el(ns, name, text string, children ...node) node {
	return node{XMLName: xml.Name{Space: ns, Local: name}, Text: text, Children: children}
}
func at(n node, key, value string) node {
	n.Attrs = append(n.Attrs, xml.Attr{Name: xml.Name{Local: key}, Value: value})
	return n
}
func (n node) attr(key string) string {
	for _, a := range n.Attrs {
		if a.Name.Space == "" && a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}
func (n node) child(ns, name string) *node {
	for i := range n.Children {
		if n.Children[i].XMLName == (xml.Name{Space: ns, Local: name}) {
			return &n.Children[i]
		}
	}
	return nil
}
func (n node) find(ns, name string) *node {
	if n.XMLName == (xml.Name{Space: ns, Local: name}) {
		return &n
	}
	for _, c := range n.Children {
		if v := c.find(ns, name); v != nil {
			return v
		}
	}
	return nil
}
func parse(raw []byte) (node, error) {
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, count, roots := 0, 0, 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return node{}, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			count++
			if depth > 48 || count > 20000 {
				return node{}, errors.New("XML limit")
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return node{}, errors.New("XML directive refused")
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return node{}, errors.New("XML outside root")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return node{}, errors.New("XML root")
	}
	var n node
	err := xml.Unmarshal(raw, &n)
	return n, err
}
func envelope(n node) []byte {
	raw, _ := xml.Marshal(el(soap, "Envelope", "", el(soap, "Body", "", n)))
	return raw
}
