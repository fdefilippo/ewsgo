// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const maxDraftRecipients = 256

type draftRecipientChange struct {
	id        uint32
	remove    bool
	recipient core.Recipient
}

func (t *SessionTransport) editStoreRecipients(ctx context.Context, session core.Session, object serverObject, op Operation, codePage uint32) uint32 {
	var draft *mailDraft
	var imported *mailImportDraft
	if object.message != nil {
		draft = object.message.mailDraft
		codePage = object.message.codePage
	} else if object.download != nil {
		imported = object.download.mailImport
	} else {
		return NotSupported
	}
	if draft == nil && imported == nil {
		return ropNotImplemented
	}
	var changes []draftRecipientChange
	if op.ID == 0x0e {
		var err error
		changes, err = decodeDraftRecipientChanges(op.ModifyRecipients, codePage, t.mailboxAddressResolver(ctx, session))
		if errors.Is(err, ErrMalformed) || errors.Is(err, core.ErrInvalid) {
			return 0x80070057
		}
		if errors.Is(err, ErrUnsupported) {
			return NotSupported
		}
		if err != nil {
			return hierarchyError(err)
		}
	}
	clearAll := op.ID == 0x0d
	if imported != nil {
		return faiError(imported.setRecipients(ctx, session, changes, clearAll), false)
	}
	return faiError(draft.setRecipients(ctx, session, changes, clearAll), false)
}

func applyRecipientChanges(existing map[uint32]core.Recipient, changes []draftRecipientChange, clearAll bool) (map[uint32]core.Recipient, error) {
	rows := make(map[uint32]core.Recipient, len(existing))
	if !clearAll {
		maps.Copy(rows, existing)
	}
	for _, change := range changes {
		if change.remove {
			delete(rows, change.id)
		} else {
			rows[change.id] = change.recipient
		}
	}
	if len(rows) > maxDraftRecipients {
		return nil, core.ErrLimit
	}
	return rows, nil
}

func installRecipientHeaders(doc *mailmime.Draft, rows map[uint32]core.Recipient) ([]core.Recipient, error) {
	list := make([]core.Recipient, 0, len(rows))
	addresses := make(map[core.RecipientKind][]string)
	for _, id := range draftRecipientIDs(rows) {
		row := rows[id]
		address, err := mailmime.SMTPAddress(row.DisplayName, row.Address)
		if err != nil {
			return nil, err
		}
		addresses[row.Kind] = append(addresses[row.Kind], address)
		list = append(list, row)
	}
	for i, field := range []string{"To", "Cc", "Bcc"} {
		value := strings.Join(addresses[core.RecipientKind(i+1)], ", ")
		if value == "" {
			doc.Header.Del(field)
		} else {
			doc.Header.Set(field, value)
		}
	}
	return list, nil
}

func (t *SessionTransport) mailboxAddressResolver(ctx context.Context, session core.Session) func(string) (string, error) {
	var directory map[string]string
	return func(dn string) (string, error) {
		if directory == nil {
			entries, code := t.visibleDirectory(ctx, session)
			if code != 0 {
				return "", core.ErrUnsupported
			}
			directory = make(map[string]string, len(entries))
			for _, entry := range entries {
				identity, err := t.namespace.Mailbox(entry.Identity)
				if err != nil {
					return "", err
				}
				directory[strings.ToLower(identity.LegacyDN)] = entry.Identity.Name
			}
		}
		address := directory[strings.ToLower(dn)]
		if address == "" {
			return "", core.ErrNotFound
		}
		return address, nil
	}
}

func decodeDraftRecipientChanges(raw []byte, cp uint32, resolve func(string) (string, error)) ([]draftRecipientChange, error) {
	r := reader{data: raw}
	columns := readRecipientUint16(&r)
	if columns > 128 {
		return nil, ErrUnsupported
	}
	tags := make([]uint32, columns)
	for i := range tags {
		tags[i] = r.u32()
	}
	count := readRecipientUint16(&r)
	if r.err != nil {
		return nil, ErrMalformed
	}
	if count > maxDraftRecipients {
		return nil, ErrUnsupported
	}
	out := make([]draftRecipientChange, 0, count)
	for range count {
		id := r.u32()
		kind := r.take(1)
		size := readRecipientUint16(&r)
		value := r.take(int(size))
		if r.err != nil {
			return nil, ErrMalformed
		}
		change := draftRecipientChange{id: id, remove: size == 0}
		if size != 0 {
			role := core.RecipientKind(kind[0])
			if role < core.RecipientTo || role > core.RecipientBcc {
				return nil, ErrUnsupported
			}
			recipient, err := decodeDraftRecipient(value, tags, role, cp, resolve)
			if err != nil {
				return nil, err
			}
			change.recipient = recipient
		}
		out = append(out, change)
	}
	if r.done() != nil {
		return nil, ErrMalformed
	}
	return out, nil
}

func readRecipientUint16(r *reader) uint16 {
	v := r.take(2)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint16(v)
}

func recipientASCII(r *reader) string {
	raw, _, found := bytes.Cut(r.data[r.pos:], []byte{0})
	if !found || len(raw) > 4096 {
		r.err = ErrMalformed
		return ""
	}
	for _, c := range raw {
		if c < 0x20 || c > 0x7e {
			r.err = ErrMalformed
			return ""
		}
	}
	r.take(len(raw) + 1)
	return string(raw)
}

func decodeDraftRecipient(raw []byte, columns []uint32, role core.RecipientKind, cp uint32, resolve func(string) (string, error)) (core.Recipient, error) {
	r := reader{data: raw}
	flags := readRecipientUint16(&r)
	if r.err != nil {
		return core.Recipient{}, ErrMalformed
	}
	routing, dn := "", ""
	switch flags & 7 {
	case 0:
		if flags&0x8000 != 0 {
			routing = recipientASCII(&r)
		}
	case 1:
		routing = "EX"
		prefix := r.take(2)
		if r.err != nil {
			return core.Recipient{}, ErrMalformed
		}
		if prefix[0] != 0 {
			return core.Recipient{}, ErrUnsupported // DN prefix only exists in bulk transfer.
		}
		dn = recipientASCII(&r)
	case 3:
		routing = "SMTP"
	default:
		return core.Recipient{}, ErrUnsupported
	}
	readString := func() (string, error) {
		if r.err != nil {
			return "", ErrMalformed
		}
		if flags&0x0200 != 0 {
			return createFolderString(&r, 2048)
		}
		v, err := (&searchRestrictionText{codePage: cp}).readString8(&r)
		return string(v), err
	}
	var address, name string
	for _, field := range []struct {
		flag   uint16
		target *string
	}{{0x08, &address}, {0x10, &name}, {0x400, nil}, {0x20, nil}} {
		if flags&field.flag == 0 {
			continue
		}
		value, err := readString()
		if err != nil {
			return core.Recipient{}, err
		}
		if field.target != nil {
			*field.target = value
		}
	}

	count := int(readRecipientUint16(&r))
	layout := r.take(1)
	if r.err != nil || count > len(columns) || layout[0] > 1 {
		return core.Recipient{}, ErrMalformed
	}
	for _, tag := range columns[:count] {
		if layout[0] == 1 {
			cell := r.take(1)
			if r.err != nil {
				return core.Recipient{}, ErrMalformed
			}
			switch cell[0] {
			case 0: // Available.
			case 1: // Absent.
				continue
			case 0x0a: // Property error.
				r.take(4)
				continue
			default:
				return core.Recipient{}, ErrMalformed
			}
		}
		kind := uint16(tag)
		if kind == 0 {
			kind = readRecipientUint16(&r)
		}
		var value core.Value
		var err error
		if kind == 0x1e {
			value, err = (&searchRestrictionText{codePage: cp}).readString8(&r)
		} else {
			value, err = readContentsValue(&r, kind)
		}
		if err != nil {
			return core.Recipient{}, err
		}
		str, _ := value.(core.Text)
		switch tag >> 16 {
		case 0x3001:
			if name == "" {
				name = string(str)
			}
		case 0x3002:
			if routing == "" {
				routing = string(str)
			}
		case 0x3003:
			if address == "" {
				address = string(str)
			}
		case 0x39fe:
			if address == "" && strings.EqualFold(routing, "SMTP") {
				address = string(str)
			}
		}
	}
	if r.done() != nil {
		return core.Recipient{}, ErrMalformed
	}
	switch strings.ToUpper(routing) {
	case "EX":
		if dn == "" {
			dn = address
		}
		var err error
		address, err = resolve(dn)
		if err != nil {
			return core.Recipient{}, err
		}
	case "SMTP":
	default:
		return core.Recipient{}, ErrUnsupported
	}
	if len(name) > 1024 || len(address) > 320 {
		return core.Recipient{}, core.ErrLimit
	}
	if _, err := mailmime.SMTPAddress(name, address); err != nil {
		return core.Recipient{}, err
	}
	return core.Recipient{Address: address, DisplayName: name, Kind: role}, nil
}
