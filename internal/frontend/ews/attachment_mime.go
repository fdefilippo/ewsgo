// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"strconv"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

type draftFile struct {
	key, name, mediaType, contentID string
	size                            int64
	inline                          bool
}

func readFileAttachment(field node) (draftMIME, error) {
	if !isElement(field, types, "FileAttachment") {
		return draftMIME{}, core.ErrUnsupported
	}
	fields := make(map[string]string)
	for _, child := range field.Children {
		name := child.XMLName.Local
		_, duplicated := fields[name]
		if child.XMLName.Space != types || len(child.Children) != 0 || duplicated {
			return draftMIME{}, core.ErrInvalid
		}
		switch name {
		case "Name", "ContentType", "ContentId", "IsInline", "Content":
			fields[name] = child.Text
		default:
			return draftMIME{}, core.ErrUnsupported
		}
	}
	name := fields["Name"]
	content, contentPresent := fields["Content"]
	if name == "" || len(name) > 255 || !mailmime.ValidHeaderValue(name) || !contentPresent {
		return draftMIME{}, core.ErrInvalid
	}
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return draftMIME{}, core.ErrInvalid
	}
	inline := false
	if value, specified := fields["IsInline"]; specified {
		inline, err = xmlBool(value)
		if err != nil {
			return draftMIME{}, core.ErrInvalid
		}
	}
	return mailmime.File(name, fields["ContentType"], fields["ContentId"], inline, data)
}

func readFileAttachments(list *node) ([]draftMIME, error) {
	if !validBatch(list) {
		return nil, core.ErrInvalid
	}
	files := make([]draftMIME, 0, len(list.Children))
	for _, field := range list.Children {
		part, err := readFileAttachment(field)
		if err != nil {
			return nil, err
		}
		files = append(files, part)
	}
	return files, nil
}

type draftPart struct {
	entity          draftMIME
	kind            string
	params          map[string]string
	children        []*draftPart
	file            *draftFile
	remove, changed bool
}

func draftAttachmentTree(doc draftMIME, revision core.Integer) (*draftPart, []*draftFile, error) {
	total := 0
	var files []*draftFile
	var walk func(draftMIME, int) (*draftPart, error)
	walk = func(entity draftMIME, depth int) (*draftPart, error) {
		total++
		if depth > 20 || total > 500 || len(files) > 100 {
			return nil, core.ErrLimit
		}
		raw := entity.Header.Get("Content-Type")
		kind, parameters, err := mime.ParseMediaType(raw)
		if err != nil && raw != "" {
			return nil, core.ErrInvalid
		}
		part := &draftPart{entity: entity, kind: kind, params: parameters}
		if depth != 0 {
			part.file, err = describeDraftFile(entity, kind, parameters)
			if err != nil {
				return nil, err
			}
			if part.file != nil {
				files = append(files, part.file)
				return part, nil
			}
		}
		if strings.HasPrefix(kind, "multipart/") {
			parts, err := entity.Parts()
			if err != nil {
				return nil, err
			}
			for _, child := range parts {
				tree, err := walk(child, depth+1)
				if err != nil {
					return nil, err
				}
				part.children = append(part.children, tree)
			}
		}
		return part, nil
	}
	root, err := walk(doc, 0)
	if err != nil || len(files) > 100 {
		if err == nil {
			err = core.ErrLimit
		}
		return nil, nil, err
	}
	counts := make(map[string]int, len(files))
	for _, file := range files {
		counts[file.key]++
	}
	occurrences := make(map[string]int)
	for _, file := range files {
		if counts[file.key] > 1 {
			// Indistinguishable legacy parts use a revision-bound occurrence.
			key := file.key
			occurrences[key]++
			file.key += ":" + strconv.FormatInt(int64(revision), 10) + ":" + strconv.Itoa(occurrences[key])
		}
	}
	return root, files, nil
}

func describeDraftFile(entity draftMIME, kind string, parameters map[string]string) (*draftFile, error) {
	raw := entity.Header.Get("Content-Disposition")
	disposition, params, err := mime.ParseMediaType(raw)
	if err != nil && raw != "" {
		return nil, core.ErrInvalid
	}
	name := params["filename"]
	if name == "" {
		name = parameters["name"]
	}
	contentID := strings.Trim(strings.TrimSpace(entity.Header.Get("Content-Id")), "<>")
	if disposition != "attachment" && name == "" &&
		(contentID == "" || strings.HasPrefix(kind, "text/") || strings.HasPrefix(kind, "multipart/")) {
		return nil, nil
	}
	if decoded, err := (&mime.WordDecoder{}).DecodeHeader(name); err == nil {
		name = decoded
	}
	if name == "" {
		name = "attachment"
	}
	if !mailmime.ValidHeaderValue(name) || !mailmime.ValidHeaderValue(contentID) {
		return nil, core.ErrInvalid
	}
	if kind == "" {
		kind = "application/octet-stream"
	}
	hash := sha256.New()
	inline := disposition != "attachment" && (disposition == "inline" || contentID != "")
	fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%t\x00", name, kind, contentID, inline)
	var body io.Reader = bytes.NewReader(entity.Body)
	switch strings.ToLower(entity.Header.Get("Content-Transfer-Encoding")) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	size, err := io.Copy(hash, body)
	if err != nil {
		return nil, core.ErrInvalid
	}
	return &draftFile{key: hex.EncodeToString(hash.Sum(nil)), name: name, mediaType: kind, contentID: contentID, size: size, inline: inline}, nil
}

func (p *draftPart) removeFiles(keys map[string]bool) bool {
	if p.file != nil {
		p.remove = keys[p.file.key]
		return p.remove
	}
	changed := false
	for _, child := range p.children {
		if child.removeFiles(keys) {
			changed = true
		}
	}
	p.changed = changed
	return changed
}

func (p *draftPart) rebuild() (draftMIME, error) {
	if !p.changed {
		return p.entity, nil
	}
	var children []draftMIME
	for _, child := range p.children {
		if child.remove {
			continue
		}
		entity, err := child.rebuild()
		if err != nil {
			return draftMIME{}, err
		}
		children = append(children, entity)
	}
	if len(children) == 0 {
		body, err := mailmime.Text("", false)
		if err != nil {
			return draftMIME{}, err
		}
		p.entity.InstallBody(body)
		return p.entity, nil
	}
	if err := p.entity.SetMultipart(p.kind, p.params, children); err != nil {
		return draftMIME{}, err
	}
	return p.entity, nil
}
