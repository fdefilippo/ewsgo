// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmime

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"mime"
	"net/textproto"
	"strings"

	"ewsgo/internal/core"
)

// File encodes the binary contents of a regular MIME file attachment.
func File(name, mediaType, contentID string, inline bool, data []byte) (Draft, error) {
	if name == "" || len(name) > 255 || !ValidHeaderValue(name) {
		return Draft{}, core.ErrInvalid
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	kind, parameters, err := mime.ParseMediaType(mediaType)
	if err != nil || !strings.Contains(kind, "/") || !ValidHeaderValue(mediaType) {
		return Draft{}, core.ErrInvalid
	}
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	contentID = strings.TrimSpace(contentID)
	if strings.HasPrefix(contentID, "<") && strings.HasSuffix(contentID, ">") {
		contentID = contentID[1 : len(contentID)-1]
	}
	if contentID == "" {
		contentID = strings.ToLower(rand.Text()) + "@ewsgo.invalid"
	}
	if len(contentID) > 255 || !ValidHeaderValue(contentID) || strings.ContainsAny(contentID, "<> \t") {
		return Draft{}, core.ErrInvalid
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Type", mime.FormatMediaType(kind, parameters))
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	header.Set("Content-Id", "<"+contentID+">")
	return binaryPart(header, data), nil
}

// RTF encodes the original rich-text body as a MIME alternative.
func RTF(data []byte) Draft {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Type", "text/rtf")
	return binaryPart(header, data)
}

func binaryPart(header textproto.MIMEHeader, data []byte) Draft {
	header.Set("Content-Transfer-Encoding", "base64")
	encoded := base64.StdEncoding.EncodeToString(data)
	var body bytes.Buffer
	for len(encoded) > 76 {
		body.WriteString(encoded[:76])
		body.WriteString("\r\n")
		encoded = encoded[76:]
	}
	body.WriteString(encoded)
	return Draft{Header: header, Body: body.Bytes()}
}
