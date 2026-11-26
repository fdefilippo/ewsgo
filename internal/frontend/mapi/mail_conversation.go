// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"crypto/md5" // MS-OXOMSG conversation identity, not a security primitive.
	"encoding/base64"
	"encoding/binary"
	"mime"
	"net/textproto"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"ewsgo/internal/core"
)

// MIME remains the source of truth. These are derived, read-only properties;
// no message IDs, native threads, dates or persistent mappings are invented.
func projectMailConversation(result *projectedMessage, h textproto.MIMEHeader) error {
	subject, hasSubject := h["Subject"]
	var topic, subjectText, prefix, normalized string
	hasTopic := false
	if hasSubject {
		if len(subject) != 0 {
			decoded, err := (&mime.WordDecoder{CharsetReader: mailCharsetReader}).DecodeHeader(subject[0])
			if err != nil || !validMailText(decoded) {
				return core.ErrUnsupported
			}
			subjectText = decoded
		}
		prefix, normalized = mailSubjectParts(subjectText)
		topic, hasTopic = normalized, true
	}
	if raw, present := h["Thread-Topic"]; present && len(raw) != 0 {
		decoded, err := (&mime.WordDecoder{CharsetReader: mailCharsetReader}).DecodeHeader(raw[0])
		if err != nil || !validMailText(decoded) {
			return core.ErrUnsupported
		}
		// Use the MIME reader's direct Thread-Topic mapping (MS-OXCMAIL
		// 2.2.3.2.7), rather than its optional header-order-dependent method.
		topic, hasTopic = decoded, true
		if hasSubject && topic != "" && strings.HasSuffix(subjectText, topic) {
			// MS-OXCMAIL also recommends using an explicit topic as a hint
			// when it matches the subject's tail, including localized prefixes.
			prefix, normalized = strings.TrimSuffix(subjectText, topic), topic
		}
	}
	// MS-OXCMSG recommends fewer than 254 characters including prefix.
	// Preserve overlong original Subject/Topic values; omit the optional
	// components rather than silently truncate message data.
	if hasSubject && len(utf16.Encode([]rune(prefix+normalized))) < 254 {
		putConversationValue(result, 0x003d001f, core.Text(prefix), len(prefix)*2)
		putConversationValue(result, 0x0e1d001f, core.Text(normalized), len(normalized)*2)
	}
	var conversationID []byte
	tracking := false
	if encoded := h.Get("Thread-Index"); encoded != "" {
		// Unfolded MIME headers can contain whitespace inside base64 text.
		encoded = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, encoded)
		if index, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(index) != 0 {
			putConversationValue(result, 0x00710102, core.Binary(index), len(index))
			if len(index) >= 22 && index[0] == 1 {
				conversationID, tracking = append([]byte(nil), index[6:22]...), true
			}
		}
	}
	if hasTopic {
		putConversationValue(result, 0x0070001f, core.Text(topic), len(topic)*2)
		if !tracking {
			conversationID = mailTopicConversationID(topic)
		}
	}
	putConversationValue(result, 0x3016000b, core.Bool(tracking), 1)
	if conversationID != nil {
		putConversationValue(result, 0x30130102, core.Binary(conversationID), len(conversationID))
	}
	return nil
}

func putConversationValue(result *projectedMessage, tag uint32, value core.Value, size int) {
	putContentsValue(result.values, tag, value)
	result.bytes += 128 + size
}

func mailSubjectParts(subject string) (prefix, normalized string) {
	colon := strings.IndexByte(subject, ':')
	if colon < 0 || utf8.RuneCountInString(subject[:colon]) < 1 || utf8.RuneCountInString(subject[:colon]) > 3 {
		return "", subject
	}
	for _, r := range subject[:colon] {
		if r == ' ' || r >= '0' && r <= '9' {
			return "", subject
		}
	}
	return subject[:colon] + ": ", strings.TrimLeft(subject[colon+1:], " ")
}

func mailTopicConversationID(topic string) []byte {
	// Locale-independent simple uppercase preserves UTF-16 unit widths.
	// MS-OXOMSG hashes at most 255 UTF-16LE units, without a terminator.
	units := utf16.Encode([]rune(strings.ToUpper(topic)))
	if len(units) > 255 {
		units = units[:255]
	}
	raw := make([]byte, 2*len(units))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(raw[i*2:], unit)
	}
	sum := md5.Sum(raw)
	return append([]byte(nil), sum[:]...)
}
