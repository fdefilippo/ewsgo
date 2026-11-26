// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmime

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"ewsgo/internal/core"
)

type Draft struct {
	Header textproto.MIMEHeader
	Body   []byte
}

// DraftReference returns an exact, unambiguous reference from a MIME envelope.
func DraftReference(raw []byte) string {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	refs := textproto.MIMEHeader(message.Header).Values(core.DraftReferenceHeader)
	if len(refs) == 1 && core.ValidDraftReference(refs[0]) {
		return refs[0]
	}
	return ""
}

func ReadDraft(raw []byte) (Draft, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Draft{}, core.ErrInvalid
	}
	body, err := io.ReadAll(message.Body)
	if err != nil {
		return Draft{}, err
	}
	return Draft{Header: textproto.MIMEHeader(message.Header), Body: body}, nil
}

func NewDraft(author string) (Draft, error) {
	from, err := SMTPAddress("", author)
	if err != nil {
		return Draft{}, err
	}
	Header := make(textproto.MIMEHeader)
	Header.Set("From", from)
	Header.Set("Mime-Version", "1.0")
	return Draft{Header: Header}, nil
}

func SMTPAddress(name, value string) (string, error) {
	if !ValidHeaderValue(name) || !ValidHeaderValue(value) {
		return "", core.ErrInvalid
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || !strings.ContainsRune(address.Address, '@') {
		return "", core.ErrInvalid
	}
	address.Name = name
	return address.String(), nil
}

func ValidHeaderValue(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, func(c rune) bool {
		return unicode.IsControl(c) && c != '\t'
	})
}

func Text(Body string, html bool) (Draft, error) {
	var encoded bytes.Buffer
	w := quotedprintable.NewWriter(&encoded)
	if _, err := io.WriteString(w, Body); err != nil {
		return Draft{}, err
	}
	if err := w.Close(); err != nil {
		return Draft{}, err
	}
	contentType := "text/plain"
	if html {
		contentType = "text/html"
	}
	Header := make(textproto.MIMEHeader)
	Header.Set("Content-Type", contentType+"; charset=utf-8")
	Header.Set("Content-Transfer-Encoding", "quoted-printable")
	return Draft{Header: Header, Body: encoded.Bytes()}, nil
}

func (d *Draft) ReplaceBody(body Draft) error {
	return d.replaceBody(body, 0)
}

func (d *Draft) replaceBody(body Draft, depth int) error {
	if depth == 20 {
		return core.ErrLimit
	}
	rawType := d.Header.Get("Content-Type")
	kind, parameters, err := mime.ParseMediaType(rawType)
	if err != nil && rawType != "" {
		return core.ErrInvalid
	}
	switch kind {
	case "multipart/mixed", "multipart/related":
		parts, err := d.Parts()
		if err != nil || len(parts) == 0 {
			return core.ErrUnsupported
		}
		if err := parts[0].replaceBody(body, depth+1); err != nil {
			return err
		}
		return d.SetMultipart(kind, parameters, parts)
	case "", "text/plain", "text/html", "multipart/alternative":
		disposition, _, _ := mime.ParseMediaType(d.Header.Get("Content-Disposition"))
		if disposition == "attachment" {
			return core.ErrUnsupported
		}
		d.InstallBody(body)
		return nil
	default:
		return core.ErrUnsupported
	}
}

func (d *Draft) InstallBody(entity Draft) {
	for _, key := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		d.Header.Del(key)
		if value := entity.Header.Get(key); value != "" {
			d.Header.Set(key, value)
		}
	}
	d.Body = entity.Body
}

func (d Draft) Parts() ([]Draft, error) {
	kind, parameters, err := mime.ParseMediaType(d.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(kind, "multipart/") || parameters["boundary"] == "" {
		return nil, core.ErrInvalid
	}
	reader := multipart.NewReader(bytes.NewReader(d.Body), parameters["boundary"])
	var result []Draft
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, core.ErrInvalid
		}
		if len(result) == 100 {
			return nil, core.ErrLimit
		}
		Body, err := io.ReadAll(part)
		if err != nil {
			return nil, core.ErrInvalid
		}
		result = append(result, Draft{Header: part.Header, Body: Body})
	}
}

func (d *Draft) SetMultipart(kind string, parameters map[string]string, parts []Draft) error {
	return d.setMultipart(kind, parameters, parts, "")
}

// SetMultipartBoundary gives callers that persist a MIME digest a stable
// representation across retries of the same operation.
func (d *Draft) SetMultipartBoundary(kind string, parts []Draft, boundary string) error {
	if boundary == "" {
		return core.ErrInvalid
	}
	return d.setMultipart(kind, nil, parts, boundary)
}

func (d *Draft) setMultipart(kind string, parameters map[string]string, parts []Draft, boundary string) error {
	var encoded bytes.Buffer
	w := multipart.NewWriter(&encoded)
	if boundary != "" {
		if err := w.SetBoundary(boundary); err != nil {
			return core.ErrInvalid
		}
		for _, part := range parts {
			if bytes.Contains(part.Body, []byte("--"+boundary)) {
				return core.ErrInvalid
			}
		}
	}
	for _, part := range parts {
		out, err := w.CreatePart(part.Header)
		if err != nil {
			return err
		}
		if _, err := out.Write(part.Body); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	d.Header.Del("Content-Transfer-Encoding")
	if parameters == nil {
		parameters = make(map[string]string)
	}
	parameters["boundary"] = w.Boundary()
	d.Header.Set("Content-Type", mime.FormatMediaType(kind, parameters))
	d.Body = encoded.Bytes()
	return nil
}

func (d Draft) Bytes() ([]byte, error) {
	var out bytes.Buffer
	keys := make([]string, 0, len(d.Header))
	for key := range d.Header {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, value := range d.Header[key] {
			if !ValidHeaderValue(value) {
				return nil, core.ErrInvalid
			}
			line := key + ": " + value
			for len(line) > 998 {
				i := strings.LastIndexAny(line[:998], " \t")
				if i <= 1 || (line[0] != '\t' && i <= len(key)) {
					return nil, core.ErrLimit
				}
				fmt.Fprint(&out, line[:i], "\r\n")
				line = "\t" + strings.TrimLeft(line[i:], " \t")
			}
			fmt.Fprint(&out, line, "\r\n")
		}
	}
	out.WriteString("\r\n")
	out.Write(d.Body)
	return out.Bytes(), nil
}

func (d *Draft) AppendFiles(files []Draft) error {
	mediaType := d.Header.Get("Content-Type")
	kind, parameters, err := mime.ParseMediaType(mediaType)
	if mediaType != "" && err != nil {
		return core.ErrInvalid
	}
	if kind == "multipart/mixed" {
		parts, err := d.Parts()
		if err != nil {
			return err
		}
		if len(parts)+len(files) > 100 {
			return core.ErrLimit
		}
		return d.SetMultipart(kind, parameters, append(parts, files...))
	}
	if len(files) >= 100 {
		return core.ErrLimit
	}
	content := Draft{Header: make(textproto.MIMEHeader), Body: d.Body}
	for key, values := range d.Header {
		if strings.HasPrefix(strings.ToLower(key), "content-") {
			content.Header[key] = values
			delete(d.Header, key)
		}
	}
	return d.SetMultipart("multipart/mixed", nil, append([]Draft{content}, files...))
}
