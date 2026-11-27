// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

const (
	messageBodyID     = 0x1000
	messageHTMLID     = 0x1013
	messageRTFTag     = 0x10090102
	messageNativeBody = 0x10160003
	messageInternetCP = 0x3fde0003
)

func messageContentPropertyTag(tag uint32) bool {
	id, kind := uint16(tag>>16), uint16(tag)
	switch id {
	case messageBodyID:
		return kind == 0 || kind == 0x001e || kind == 0x001f
	case messageHTMLID, messageRTFTag >> 16:
		return kind == 0 || kind == 0x0102
	case messageNativeBody >> 16, messageInternetCP >> 16:
		return kind == 0 || kind == 0x0003
	default:
		return false
	}
}

func variableMessageProperty(tag uint32) bool {
	id := uint16(tag >> 16)
	return isAssociatedSpecificBinary(tag) || (id == messageBodyID || id == messageHTMLID || id == messageRTFTag>>16) && messageContentPropertyTag(tag)
}

// Resolve and check the Message under the current mailbox authorization before
// fetching MIME. The content revision is the one established at OpenMessage.
func (s *ramStore) readOpenedMailProjection(ctx context.Context, session core.Session, fid uint64, message openedMessage) (projectedMessage, error) {
	if message.mailDraft != nil {
		if err := message.mailDraft.authorize(ctx, session); err != nil {
			return projectedMessage{}, err
		}
		return projectMailContents(message.mailDraft.staged)
	}
	root := message.rootMessage()
	key, folder, err := s.openedMessageScope(ctx, session, fid, root)
	if err != nil {
		return projectedMessage{}, err
	}
	if key.associated || key.special != 0 {
		return projectedMessage{}, core.ErrUnsupported
	}
	object, err := readOpenedMessageMIME(ctx, session, folder, key, root)
	if err != nil {
		return projectedMessage{}, err
	}
	projection, err := projectReadableMailContents(object)
	if err != nil {
		return projectedMessage{}, err
	}
	if ref := message.embedded; ref != nil {
		for _, number := range ref.path[:ref.depth] {
			child, exists := projection.embedded[int(number)]
			if !exists {
				return projectedMessage{}, core.ErrNotFound
			}
			projection = child
		}
	}
	return projection, ctx.Err()
}

// Return the native byte representation used both inline and in property
// Streams. Unicode and String8 include the terminator. HTML is binary and
// keeps the MIME part's original code page.
func messageBodyBytes(values contentsValues, tag uint32, cp uint32) ([]byte, bool) {
	switch tag {
	case messageBodyID<<16 | 0x001e, messageBodyID<<16 | 0x001f:
		text, ok := values[fixedContentsProperty(messageBodyID<<16|0x001f)].(core.Text)
		if !ok {
			return nil, false
		}
		if uint16(tag) == 0x001f {
			return contentsUnicode(string(text)), true
		}
		return propertyString8(string(text), cp)
	case messageHTMLID<<16 | 0x0102, messageRTFTag:
		value, ok := values[fixedContentsProperty(tag)].(core.Binary)
		return value, ok
	default:
		return nil, false
	}
}

func openedMailContentProperty(values contentsValues, tag uint32, unicode bool, cp uint32, budget int) folderPropertyValue {
	id, kind := uint16(tag>>16), uint16(tag)
	if kind == 0 {
		switch id {
		case messageBodyID:
			kind = 0x001e
			if unicode {
				kind = 0x001f
			}
		case messageHTMLID, messageRTFTag >> 16:
			kind = 0x0102
		default:
			kind = 0x0003
		}
		tag |= uint32(kind)
	}
	if kind == 3 {
		if value, ok := values[fixedContentsProperty(tag)].(core.Integer); ok {
			return folderPropertyValue{kind: kind, data: append32(nil, uint32(value))}
		}
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	}
	data, ok := messageBodyBytes(values, tag, cp)
	if !ok {
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	}
	size := len(data)
	if kind == 0x0102 {
		size += 2
	}
	// The client can retrieve a present but oversized body using OpenStream.
	if size > budget+4 || len(data) > 65535 {
		return folderPropertyValue{kind: 0x000a, code: 0x8007000e}
	}
	if kind == 0x0102 {
		data = append(binary.LittleEndian.AppendUint16(nil, uint16(len(data))), data...)
	}
	return folderPropertyValue{kind: kind, data: data}
}
