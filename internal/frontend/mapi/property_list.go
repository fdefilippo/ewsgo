// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"slices"
)

const ropGetPropertiesAll byte = 0x08

// These candidates describe existing GetPropertiesSpecific projections.
// Enumeration returns only tags whose values the current object can supply.
var (
	logonPropertyTags  = []uint32{logonUserEntryID<<16 | 0x0102, logonOwnerEntryID<<16 | 0x0102, 0x661c001f}
	folderPropertyTags = []uint32{
		folderHiddenTag, folderAccessTag, 0x3001001f, 0x3004001f, 0x36010003, 0x36020003, 0x36030003, 0x360a000b,
		0x3613001f, remindersOnlineEntryID<<16 | 0x0102, 0x36d81102, 0x36d90102, 0x36da0102, 0x36de0003, 0x66380003,
		0x670a0040, 0x670b0003, 0x67480014, 0x67490014, folderEntryIDTag, parentFolderEntryIDTag, ostPropertyTag, freeBusyEntryIDsTag,
	}
	messagePropertyTags = []uint32{
		0x001a001f, 0x0037001f, 0x003d001f, 0x0e060040, 0x0e1d001f, 0x0ff70003,
		0x30080040, 0x65e20102, 0x67480014, 0x674a0014,
		0x0e070003, 0x0e080003, 0x0e170003, 0x0e1b000b, 0x0e69000b, folderEntryIDTag, parentFolderEntryIDTag,
		0x0ff90102, 0x0ffa0102, 0x0ffe0003, 0x65e00102,
		flagStatusTag, flagCompleteTimeTag, followupIconTag,
		messageBodyID<<16 | 0x001f, messageHTMLID<<16 | 0x0102, messageRTFTag, messageNativeBody, messageInternetCP,
	}
	faiPropertyTags = []uint32{0x65e20102}
)

func propertyReadResponse(op Operation, values []folderPropertyValue) []byte {
	if op.ID == ropGetPropertiesAll {
		return taggedPropertiesResponse(op, values)
	}
	if op.ID != 0x09 {
		return append(ropError(op.ID, op.Input, 0), folderPropertyRow(*op.Properties, values)...)
	}
	tags := make([]uint32, 0, len(values))
	for i, value := range values {
		if value.code == 0 {
			tags = append(tags, op.Properties.tags[i])
		}
	}
	return propertyTagsResponse(op, tags)
}

func enumeratedPropertyTags(tags []uint32, unicode bool) []uint32 {
	tags = slices.Clone(tags)
	for i, tag := range tags {
		if !unicode && uint16(tag) == 0x001f {
			tags[i] = tag&0xffff0000 | 0x001e
		}
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}

func taggedPropertiesResponse(op Operation, values []folderPropertyValue) []byte {
	response := append(ropError(op.ID, op.Input, 0), 0, 0)
	var count uint16
	for i, value := range values {
		switch value.code {
		case PropertyNotFound, NotSupported:
			continue // The candidate did not identify a property on this object.
		case 0:
		default:
			value.kind = 0x000a
			value.data = append32(nil, value.code)
		}
		if len(response)+4+len(value.data) > op.PropertyListLimit {
			return ropError(op.ID, op.Input, tableBufferTooSmall)
		}
		response = append32(response, op.Properties.tags[i]&0xffff0000|uint32(value.kind))
		response = append(response, value.data...)
		count++
	}
	binary.LittleEndian.PutUint16(response[6:], count)
	return response
}

func propertyTagsResponse(op Operation, tags []uint32) []byte {
	if 8+4*len(tags) > op.PropertyListLimit {
		return ropError(op.ID, op.Input, tableBufferTooSmall)
	}
	slices.Sort(tags)
	out := binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), uint16(len(tags)))
	for _, tag := range tags {
		out = append32(out, tag)
	}
	return out
}
