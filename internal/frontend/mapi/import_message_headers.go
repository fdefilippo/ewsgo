// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"net/textproto"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const oneOffProvider = "\x81\x2b\x1f\xa4\xbe\xa3\x10\x19\x9d\x6e\x00\xdd\x01\x0f\x54\x02"

func importMailHeaderTag(tag uint32) bool {
	switch tag {
	case 0x003d001f, 0x0e1d001f, 0x1035001f, 0x00390040,
		0x00410102, 0x0042001f, 0x0064001f, 0x0065001f, 0x5d02001f,
		0x0c190102, 0x0c1a001f, 0x0c1e001f, 0x0c1f001f, 0x5d01001f:
		return true
	default:
		return false
	}
}

func importMailHeaders(headers string, props map[uint32]core.Value, resolve func(string) (string, error)) (mailmime.Draft, error) {
	if headers != "" {
		block := strings.TrimRight(headers, "\r\n") + "\r\n\r\n"
		doc, err := mailmime.ReadDraft([]byte(block))
		if err != nil || len(doc.Body) != 0 || doc.Header.Get("From") == "" || doc.Header.Get("Message-Id") == "" {
			return mailmime.Draft{}, core.ErrInvalid
		}
		return doc, nil
	}
	doc := mailmime.Draft{Header: make(textproto.MIMEHeader)}
	from, err := importMailAddress(props, resolve, 0x0042001f, 0x0064001f, 0x0065001f, 0x5d02001f, 0x00410102)
	if err != nil {
		return mailmime.Draft{}, err
	}
	sender, err := importMailAddress(props, resolve, 0x0c1a001f, 0x0c1e001f, 0x0c1f001f, 0x5d01001f, 0x0c190102)
	if err != nil {
		return mailmime.Draft{}, err
	}
	if from == "" {
		from = sender
	}
	if from == "" {
		return mailmime.Draft{}, core.ErrUnsupported
	}
	doc.Header.Set("From", from)
	if sender != "" && sender != from {
		doc.Header.Set("Sender", sender)
	}
	if value := importHeaderString(props, 0x1035001f); value != "" {
		id := strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">")
		if !core.ValidInternetMessageID(id) || (value != id && value != "<"+id+">") {
			return mailmime.Draft{}, core.ErrInvalid
		}
		doc.Header.Set("Message-Id", "<"+id+">")
	}
	if date, ok := props[0x00390040].(core.Timestamp); ok {
		stamp := time.Time(date).UTC()
		if stamp.Year() < 1900 || stamp.Year() > 9999 {
			return mailmime.Draft{}, core.ErrInvalid
		}
		doc.Header.Set("Date", stamp.Format(time.RFC1123Z))
	}
	return doc, nil
}

func importHeaderString(props map[uint32]core.Value, tag uint32) string {
	value, _ := props[tag].(core.Text)
	return string(value)
}

func importMailAddress(props map[uint32]core.Value, resolve func(string) (string, error), nameTag, typeTag, emailTag, smtpTag, entryTag uint32) (string, error) {
	name := importHeaderString(props, nameTag)
	addressType := importHeaderString(props, typeTag)
	email := importHeaderString(props, emailTag)
	smtp := importHeaderString(props, smtpTag)
	if smtp == "" && email != "" {
		switch {
		case strings.EqualFold(addressType, "SMTP"):
			smtp = email
		case strings.EqualFold(addressType, "EX") && resolve != nil:
			var err error
			smtp, err = resolve(email)
			if err != nil {
				return "", err
			}
		}
	}
	if smtp == "" {
		if entry, ok := props[entryTag].(core.Binary); ok {
			entryName, address, err := importEntryAddress(entry, resolve)
			if err != nil {
				return "", err
			}
			smtp = address
			if name == "" {
				name = entryName
			}
		}
	}
	if smtp == "" {
		if name != "" || email != "" || addressType != "" {
			return "", core.ErrUnsupported
		}
		return "", nil
	}
	return mailmime.SMTPAddress(name, smtp)
}

// MS-OXCDATA 2.2.5.1 and 2.2.5.2. Only the visible directory supplies an
// address for an Exchange DN. A one-off SMTP address is self-contained.
func importEntryAddress(entry []byte, resolve func(string) (string, error)) (name, address string, err error) {
	if len(entry) < 20 || binary.LittleEndian.Uint32(entry) != 0 {
		return "", "", core.ErrInvalid
	}
	switch string(entry[4:20]) {
	case oneOffProvider:
		if len(entry) < 24 || binary.LittleEndian.Uint16(entry[20:]) != 0 {
			return "", "", core.ErrInvalid
		}
		// The flags word is little-endian. MAPI_ONE_OFF_UNICODE is 0x8000.
		unicode := binary.LittleEndian.Uint16(entry[22:])&0x8000 != 0
		r := reader{data: entry[24:]}
		var routing string
		for _, field := range []*string{&name, &routing, &address} {
			if unicode {
				*field, err = createFolderString(&r, 1024)
			} else {
				*field = recipientASCII(&r)
			}
			if err != nil || r.err != nil || len(*field) > 1024 {
				return "", "", core.ErrInvalid
			}
		}
		if r.done() != nil || address == "" {
			return "", "", core.ErrInvalid
		}
		switch {
		case strings.EqualFold(routing, "SMTP"):
			return name, address, nil
		case strings.EqualFold(routing, "EX") && resolve != nil:
			address, err = resolve(address)
			return name, address, err
		}
	case mailUserEntryIDHeader[4:20]:
		if !isMailUserEntryID(entry) {
			return "", "", core.ErrUnsupported
		}
		if resolve != nil {
			dn := string(entry[len(mailUserEntryIDHeader) : len(entry)-1])
			address, err = resolve(dn)
			return "", address, err
		}
	}
	return "", "", core.ErrUnsupported
}
