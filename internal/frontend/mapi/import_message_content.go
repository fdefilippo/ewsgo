// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/textproto"
	"strconv"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const maxMessageImportBytes = 512 << 10

func importedMailPropertyTag(tag uint32) bool {
	switch tag {
	case 0x001a001f, 0x0037001f, 0x007d001f, 0x1000001f, 0x10130102, 0x0e070003, 0x0e1b000b,
		messageInternetCP, messageNativeBody, messageRTFTag, flagStatusTag, flagCompleteTimeTag, followupIconTag:
		return true
	default:
		return importMailHeaderTag(tag)
	}
}

// Decode normal IPM.Note messageContent before Save can produce native effects.
func importedMailMIME(raw []byte, recipients map[uint32]core.Recipient, resolve func(string) (string, error)) ([]byte, core.MailAppendOptions, error) {
	if len(raw) == 0 || len(raw) > maxMessageImportBytes {
		return nil, core.MailAppendOptions{}, core.ErrInvalid
	}
	var headers, subject string
	var plain, html *string
	var compressedRTF, richRTF []byte
	var nativeBody, internetCodePage core.Integer
	var haveFlags, hasAttach, flagsHaveAttach bool
	var flagStatus, flagIcon core.Integer
	var options core.MailAppendOptions
	var attachments []mailmime.Draft
	numbers := make(map[uint32]bool)
	seen := make(map[uint32]bool)
	headerProperties := make(map[uint32]core.Value)
	salt := sha256.Sum256(raw)
	r := reader{data: raw}
	for r.pos < len(raw) {
		tag := r.u32()
		if r.err != nil {
			return nil, core.MailAppendOptions{}, core.ErrInvalid
		}
		if tag == 0x40160003 { // Replace a complete recipient or attachment set.
			target := r.u32()
			if r.err != nil || target != 0x0e12000d && target != 0x0e13000d {
				return nil, core.MailAppendOptions{}, core.ErrUnsupported
			}
			if seen[target] || len(attachments) != 0 {
				return nil, core.MailAppendOptions{}, core.ErrInvalid
			}
			seen[target] = true
			continue
		}
		if tag == 0x40000003 { // NewAttach.
			if len(attachments) == maxMailAttachments {
				return nil, core.MailAppendOptions{}, core.ErrLimit
			}
			part, number, err := readImportedAttachment(&r, salt)
			if err != nil {
				return nil, core.MailAppendOptions{}, err
			}
			if numbers[number] {
				return nil, core.MailAppendOptions{}, core.ErrInvalid
			}
			numbers[number] = true
			attachments = append(attachments, part)
			continue
		}
		if seen[tag] {
			return nil, core.MailAppendOptions{}, core.ErrInvalid
		}
		seen[tag] = true
		if !importedMailPropertyTag(tag) {
			return nil, core.MailAppendOptions{}, core.ErrUnsupported
		}
		value, err := readMessageImportProperty(&r, uint16(tag))
		if err != nil {
			return nil, core.MailAppendOptions{}, err
		}
		switch tag {
		case 0x001a001f:
			if value != core.Text("IPM.Note") {
				return nil, core.MailAppendOptions{}, core.ErrUnsupported
			}
		case 0x0037001f:
			subject = string(value.(core.Text))
		case 0x007d001f:
			headers = string(value.(core.Text))
		case 0x1000001f:
			body := string(value.(core.Text))
			plain = &body
		case 0x10130102:
			body := string(value.(core.Binary))
			if !validMailText(body) {
				return nil, core.MailAppendOptions{}, core.ErrInvalid
			}
			html = &body
		case messageRTFTag:
			compressedRTF = value.(core.Binary)
		case 0x0e070003:
			flags := value.(core.Integer)
			if flags&(8|64) != 0 { // Unsent, Associated.
				return nil, core.MailAppendOptions{}, core.ErrUnsupported
			}
			options.Read, haveFlags = flags&1 != 0, true
			flagsHaveAttach = flags&16 != 0
		case 0x0e1b000b:
			hasAttach = value == core.Bool(true)
		case flagStatusTag:
			flagStatus = value.(core.Integer)
		case followupIconTag:
			flagIcon = value.(core.Integer)
		case flagCompleteTimeTag:
			return nil, core.MailAppendOptions{}, core.ErrUnsupported
		case messageInternetCP:
			internetCodePage = value.(core.Integer)
		case messageNativeBody:
			nativeBody = value.(core.Integer)
			if nativeBody < 0 || nativeBody > 3 {
				return nil, core.MailAppendOptions{}, core.ErrUnsupported
			}
		default:
			headerProperties[tag] = value
		}
	}
	if !haveFlags ||
		(seen[0x0e1b000b] && hasAttach != (len(attachments) != 0)) || (flagsHaveAttach && len(attachments) == 0) {
		return nil, core.MailAppendOptions{}, core.ErrUnsupported
	}
	// Zimbra can set the active flag atomically when appending. Completion and
	// other icon states require metadata that this append contract cannot save.
	options.Flagged = flagStatus == 2
	if (flagStatus != 0 && flagStatus != 2) || (flagIcon != 0 && (!options.Flagged || flagIcon != 6)) {
		return nil, core.MailAppendOptions{}, core.ErrUnsupported
	}
	usesRTF := seen[messageRTFTag] && (nativeBody == 2 || plain == nil && html == nil)
	if internetCodePage != 0 && internetCodePage != 65001 && internetCodePage != 20127 && !(usesRTF && internetCodePage == 1252) {
		return nil, core.MailAppendOptions{}, core.ErrUnsupported
	}
	if headers == "" && !usesRTF {
		return nil, core.MailAppendOptions{}, core.ErrUnsupported
	}
	if usesRTF {
		rawRTF, err := uncompressImportRTF(compressedRTF)
		if err != nil {
			return nil, core.MailAppendOptions{}, err
		}
		body, err := readImportRTF(rawRTF)
		if err != nil {
			return nil, core.MailAppendOptions{}, err
		}
		if plain != nil && *plain != body.plain {
			return nil, core.MailAppendOptions{}, core.ErrUnsupported
		}
		plain = &body.plain
		if body.rich {
			richRTF = rawRTF
		}
	}
	if plain == nil && html == nil || nativeBody == 2 && !seen[messageRTFTag] {
		return nil, core.MailAppendOptions{}, core.ErrUnsupported
	}
	doc, err := importMailHeaders(headers, headerProperties, resolve)
	if err != nil {
		return nil, core.MailAppendOptions{}, err
	}
	if recipients != nil {
		if _, err := installRecipientHeaders(&doc, recipients); err != nil {
			return nil, core.MailAppendOptions{}, err
		}
	}
	doc.Header.Del(core.DraftReferenceHeader)
	doc.Header.Del("Content-Length")
	doc.Header.Set("Mime-Version", "1.0")
	if seen[0x0037001f] {
		if !mailmime.ValidHeaderValue(subject) {
			return nil, core.MailAppendOptions{}, core.ErrInvalid
		}
		doc.Header.Set("Subject", mime.QEncoding.Encode("utf-8", subject))
	} else if headers == "" {
		subject := importHeaderString(headerProperties, 0x003d001f) + importHeaderString(headerProperties, 0x0e1d001f)
		if !mailmime.ValidHeaderValue(subject) {
			return nil, core.MailAppendOptions{}, core.ErrInvalid
		}
		doc.Header.Set("Subject", mime.QEncoding.Encode("utf-8", subject))
	}
	body, err := importedMailBody(plain, html, richRTF, attachments, salt)
	if err != nil {
		return nil, core.MailAppendOptions{}, err
	}
	doc.Header.Del("Content-Disposition")
	doc.InstallBody(body)
	encoded, err := doc.Bytes()
	if len(encoded) > core.MaxMailContentMIMEBytes {
		return nil, core.MailAppendOptions{}, core.ErrLimit
	}
	return encoded, options, err
}

func importedMailBody(plain, html *string, richRTF []byte, files []mailmime.Draft, salt [32]byte) (mailmime.Draft, error) {
	var parts []mailmime.Draft
	for _, text := range []struct {
		content *string
		html    bool
	}{{plain, false}, {html, true}} {
		if text.content == nil {
			continue
		}
		part, err := mailmime.Text(*text.content, text.html)
		if err != nil {
			return mailmime.Draft{}, err
		}
		parts = append(parts, part)
	}
	if richRTF != nil {
		parts = append(parts, mailmime.RTF(richRTF))
	}
	body := parts[0]
	if len(parts) > 1 {
		body = mailmime.Draft{Header: make(textproto.MIMEHeader)}
		if err := body.SetMultipartBoundary("multipart/alternative", parts, "alt-"+hex.EncodeToString(salt[:])); err != nil {
			return mailmime.Draft{}, err
		}
	}
	if len(files) == 0 {
		return body, nil
	}
	mixed := mailmime.Draft{Header: make(textproto.MIMEHeader)}
	if err := mixed.SetMultipartBoundary("multipart/mixed", append([]mailmime.Draft{body}, files...), "mixed-"+hex.EncodeToString(salt[:])); err != nil {
		return mailmime.Draft{}, err
	}
	return mixed, nil
}

func readImportedAttachment(r *reader, salt [32]byte) (mailmime.Draft, uint32, error) {
	if r.u32() != attachNumberTag {
		return mailmime.Draft{}, 0, core.ErrInvalid
	}
	number := r.u32()
	if r.err != nil || number > 0x7fffffff {
		return mailmime.Draft{}, 0, core.ErrInvalid
	}
	var name, shortName, media, cid, location string
	var data []byte
	var haveData, inline bool
	seen := make(map[uint32]bool)
	for {
		tag := r.u32()
		if r.err != nil || seen[tag] {
			return mailmime.Draft{}, 0, core.ErrInvalid
		}
		if tag == 0x400e0003 { // EndAttach.
			break
		}
		switch tag {
		case attachDataBinary, 0x37050003, 0x3707001f, 0x3704001f, 0x370e001f, 0x370b0003,
			0x3712001f, 0x3713001f, 0x37140003, 0x0e200003, 0x3703001f:
		default:
			return mailmime.Draft{}, 0, core.ErrUnsupported
		}
		seen[tag] = true
		value, err := readMessageImportProperty(r, uint16(tag))
		if err != nil {
			return mailmime.Draft{}, 0, err
		}
		switch tag {
		case attachDataBinary:
			data, haveData = value.(core.Binary), true
		case 0x37050003:
			if value != core.Integer(1) { // AttachByValue.
				return mailmime.Draft{}, 0, core.ErrUnsupported
			}
		case 0x3707001f:
			name = string(value.(core.Text))
		case 0x3704001f:
			shortName = string(value.(core.Text))
		case 0x370e001f:
			media = string(value.(core.Text))
		case 0x370b0003:
			if value != core.Integer(-1) {
				return mailmime.Draft{}, 0, core.ErrUnsupported
			}
		case 0x3712001f:
			cid = string(value.(core.Text))
		case 0x3713001f:
			location = string(value.(core.Text))
			if !mailmime.ValidHeaderValue(location) {
				return mailmime.Draft{}, 0, core.ErrInvalid
			}
		case 0x37140003:
			flags := value.(core.Integer)
			if flags != 0 && flags != 4 { // ATT_MHTML_REF.
				return mailmime.Draft{}, 0, core.ErrUnsupported
			}
			inline = flags == 4
		}
	}
	kind, _, _ := mime.ParseMediaType(media)
	if !haveData || strings.HasPrefix(kind, "multipart/") {
		return mailmime.Draft{}, 0, core.ErrUnsupported
	}
	if name == "" {
		name = shortName
	}
	if cid == "" {
		cid = hex.EncodeToString(salt[:16]) + "." + strconv.FormatUint(uint64(number), 10) + "@ewsgo.invalid"
	}
	part, err := mailmime.File(name, media, cid, inline, data)
	if err == nil && location != "" {
		part.Header.Set("Content-Location", location)
	}
	return part, number, err
}

func readMessageImportProperty(r *reader, kind uint16) (core.Value, error) {
	if kind != 0x001f && kind != 0x0102 {
		return readFAITransferValue(r, kind)
	}
	size := r.u32()
	raw := r.take(int(size))
	if r.err != nil || len(raw) > maxMessageImportBytes {
		return nil, core.ErrInvalid
	}
	if kind == 0x0102 {
		return core.Binary(raw), nil
	}
	text := reader{data: raw}
	value, err := createFolderString(&text, len(raw)/2)
	if err != nil || text.done() != nil {
		return nil, core.ErrInvalid
	}
	return core.Text(value), nil
}
