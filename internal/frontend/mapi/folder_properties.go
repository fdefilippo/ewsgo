// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"slices"

	"ewsgo/internal/core"
)

const maxFolderPropertyTags = 256
const maxFolderStringBytes = 4096
const folderAccessTag uint32 = 0x0ff40003

const (
	folderRightsTag      uint32 = 0x66390003
	folderFlagsTag       uint32 = 0x66a80003
	ownerFolderRights           = 0x07fb
	ipmNormalFolderFlags        = 0x01 | 0x04
)

type specificProperties struct {
	sizeLimit          uint16 // MS-OXCPRPT 3.2.5.1 recommends ignoring this field.
	wantUnicode        bool
	hasVariableMessage bool
	variableBudget     int
	tags               []uint32
}

func parseSpecificProperties(r *reader) (specificProperties, error) {
	head := r.take(6)
	if r.err != nil {
		return specificProperties{}, r.err
	}
	count := int(binary.LittleEndian.Uint16(head[4:]))
	// The validated ROP buffer limits the array. Reserve response space
	// separately before any backend reads or handle changes.
	wire := r.take(count * 4)
	if r.err != nil {
		return specificProperties{}, r.err
	}
	out := specificProperties{sizeLimit: binary.LittleEndian.Uint16(head), wantUnicode: binary.LittleEndian.Uint16(head[2:]) != 0, tags: make([]uint32, count)}
	for i := range out.tags {
		out.tags[i] = binary.LittleEndian.Uint32(wire[i*4:])
	}
	return out, nil
}

// A worst-case flagged row, including resolved types, is reserved before any
// native read or batch handle mutation. The encoded string cap is independent
// of PropertySizeLimit. Duplicate columns retain their individual bounds.
func specificPropertiesBound(request specificProperties, ostMaximum int) int {
	return specificPropertiesMetadataBound(request, ostMaximum, nil)
}

func specificPropertiesMetadataBound(request specificProperties, ostMaximum int, metadata *metadataBatchView) int {
	return specificPropertiesObjectBound(request, ostMaximum, metadata, false)
}

func specificPropertiesSlotBound(request specificProperties, ostMaximum int, metadata *metadataBatchView, folder, message bool) int {
	if !message {
		return specificPropertiesObjectBound(request, ostMaximum, metadata, false)
	}
	bound := specificPropertiesObjectBound(request, ostMaximum, metadata, true)
	if folder {
		bound = max(bound, specificPropertiesObjectBound(request, ostMaximum, metadata, false))
	}
	return bound
}

func specificPropertiesObjectBound(request specificProperties, ostMaximum int, metadata *metadataBatchView, message bool) int {
	n := 7 // ROP header and PropertyRow flag.
	for _, tag := range request.tags {
		size := 4 // Per-property error value.
		kind, id := uint16(tag), uint16(tag>>16)
		switch {
		case id == 0x65e2 && (kind == 0 || kind == 0x0102):
			size = 2 + 24 // PidTagChangeKey: GUID plus at most eight local bytes.
		case message && id == 0x65e0 && (kind == 0 || kind == 0x0102):
			size = 2 + 22
		case (id >= 0x36d0 && id <= 0x36d4 || id == 0x36d7) && (kind == 0 || kind == 0x102):
			size = 48
		case !message && id == uint16(freeBusyEntryIDsTag>>16) && (kind == 0 || kind == 0x1102):
			size = 4 + 4*2 + folderEntryIDBytes + messageEntryIDBytes
		case message && id == folderEntryIDTag>>16 && (kind == 0 || kind == 0x102):
			size = 2 + messageEntryIDBytes
		case message && id == 0x0ff9 && (kind == 0 || kind == 0x102):
			size = 2 + folderEntryIDBytes
		case message && id == 0x0ffa && (kind == 0 || kind == 0x102):
			size = 2 + 16
		case (id == folderEntryIDTag>>16 || id == parentFolderEntryIDTag>>16) && (kind == 0 || kind == 0x102):
			size = 2 + folderEntryIDBytes
		case ostMaximum >= 0 && id == 0x7c04 && (kind == 0 || kind == 0x0102):
			size = max(4, 2+ostMaximum)
		case (id == 0x3001 || id == 0x3004 || id == 0x3613) && (kind == 0 || kind == 0x001e || kind == 0x001f):
			size = maxFolderStringBytes
		case message && (id == 0x001a || id == 0x0037 || id == 0x003d || id == 0x0e1d) && (kind == 0 || kind == 0x001e || kind == 0x001f):
			size = maxFolderStringBytes
		case message && associatedSpecificPropertyType(id) == 0x001f && (kind == 0 || kind == 0x001e || kind == 0x001f):
			size = maxFolderStringBytes
		case id == 0x661c && (kind == 0 || kind == 0x001e || kind == 0x001f):
			size = maxLogonNameBytes
		case logonAddressEntryTag(id, kind):
			size = 2 + maxOwnAddressBookEntryID
		case (id == 0x6748 || id == 0x6749 || message && id == 0x674a) && (kind == 0 || kind == 0x0014):
			size = 8
		case !message && id == localCommitTimeMaxID && (kind == 0 || kind == 0x0040):
			size = 8
		case message && (id == 0x0e06 || id == 0x3007 || id == 0x3008) && (kind == 0 || kind == 0x0040):
			size = 8
		case message && (id == appointmentStartDateTag>>16 || id == appointmentEndDateTag>>16) && (kind == 0 || kind == 0x0040):
			size = 8
		case message && id >= 0x8001 && id <= 0xfffe && (kind == 0 || kind == 0x0040):
			size = 8 // Named properties can hold a FILETIME.
		}
		size = max(size, metadata.bound(tag))
		n += 1 + size // FlaggedPropertyValue flag.
		if kind == 0 {
			n += 2
		}
	}
	return n
}

// StorePropertyDiagnostic contains fixed public names, verified object kinds
// and bounded counts. The separate laboratory opt-in retains at most sixteen
// filtered technical tags; values/identities cannot enter either diagnostic.
// Counts aggregate actually attempted ROPs in a batch.
type StorePropertyDiagnostic struct {
	propertyNames                              [40]bool
	unknown                                    unknownPropertyTrace
	Folder, Logon, Message                     bool
	Requests, Requested, Values, Errors, Other int
}

func (StorePropertyDiagnostic) String() string   { return "StorePropertyDiagnostic{redacted}" }
func (StorePropertyDiagnostic) GoString() string { return "StorePropertyDiagnostic{redacted}" }

func (d *StorePropertyDiagnostic) request(tags []uint32) {
	d.Requests++
	d.Requested += len(tags)
	for _, tag := range tags {
		index := publicStorePropertyIndex(uint16(tag >> 16))
		if index < 0 {
			d.Other++
			d.unknown.record(tag)
		} else {
			d.propertyNames[index] = true
		}
	}
}

type folderPropertyValue struct {
	kind uint16
	data []byte
	code uint32
}

func projectFolderProperty(folder storeFolder, tag uint32, unicode bool, cp uint32) folderPropertyValue {
	return projectFolderPropertyWithClass(folder, tag, unicode, cp, metadataClass(folder.properties))
}
func projectFolderPropertyWithClass(folder storeFolder, tag uint32, unicode bool, cp uint32, class string) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0x000a, code: code} }
	id, kind := uint16(tag>>16), uint16(tag)
	switch id {
	case localCommitTimeMaxID, deletedCountTotalID:
		if !isFolderAggregateTag(tag) {
			return fail(NotSupported)
		}
		if folder.aggregateCode != 0 {
			return fail(folder.aggregateCode)
		}
		if folder.aggregate.committed == 0 {
			return fail(NotSupported)
		}
		if id == localCommitTimeMaxID {
			return folderPropertyValue{kind: 0x0040, data: binary.LittleEndian.AppendUint64(nil, folder.aggregate.committed)}
		}
		return folderPropertyValue{kind: 0x0003, data: append32(nil, folder.aggregate.removed)}
	case 0x3601:
		if kind != 0 && kind != 3 {
			return fail(NotSupported)
		}
		var folderType uint32
		if folder.parent != 0 {
			folderType = childFolderType(folder.search)
		}
		return folderPropertyValue{kind: 3, data: append32(nil, folderType)}
	case 0x3602, 0x3603:
		if kind != 0 && kind != 3 {
			return fail(NotSupported)
		}
		counts, err := readFolderCounters(folder.properties)
		if err != nil {
			return fail(storeError(err))
		}
		index := int(id - 0x3602)
		if !counts.present[index] {
			return fail(PropertyNotFound)
		}
		return folderPropertyValue{kind: 3, data: append32(nil, counts.values[index])}
	case 0x3613:
		if class == "" {
			return fail(NotSupported)
		}
		proxy := folder
		proxy.properties = core.Properties{core.DisplayName: core.Text(class)}
		return projectFolderProperty(proxy, 0x30010000|uint32(kind), unicode, cp)
	case 0x3001, 0x3004:
		if kind == 0 {
			kind = 0x001e
			if unicode {
				kind = 0x001f
			}
		}
		if kind != 0x001e && kind != 0x001f {
			return fail(NotSupported)
		}
		name, ok := folder.properties[core.DisplayName].(core.Text)
		if id == 0x3004 {
			name, ok = core.Text(folder.comment), folder.dynamic
		}
		if !ok || (name == "" && id != 0x3004) {
			return fail(PropertyNotFound)
		}
		if kind == 0x001e && cp != 1252 && cp != 65001 {
			return fail(0x8004011e) // Unknown MBCS code page; never misencode.
		}
		data, ok := propertyString(string(name), kind, cp)
		if !ok {
			return fail(0x80070057)
		}
		if len(data) > maxFolderStringBytes {
			return fail(0x8007000e) // Local resource bound; no silent truncation.
		}
		return folderPropertyValue{kind: kind, data: data}
	case 0x6638: // PidTagFolderChildCount, computed from the authorized projection.
		if kind != 0 && kind != 3 {
			return fail(NotSupported)
		}
		if !folder.childrenKnown {
			return fail(NotSupported)
		}
		return folderPropertyValue{kind: 3, data: append32(nil, uint32(folder.childCount))}
	case 0x360a: // PidTagSubfolders.
		if kind != 0 && kind != 0x000b {
			return fail(NotSupported)
		}
		if !folder.childrenKnown {
			return fail(NotSupported)
		}
		var present byte
		if folder.childCount > 0 {
			present = 1
		}
		return folderPropertyValue{kind: 0x000b, data: []byte{present}}
	case 0x6748, 0x6749:
		if kind != 0 && kind != 0x0014 {
			return fail(NotSupported)
		}
		fid := folder.fid
		if id == 0x6749 {
			fid = folder.parent
		}
		if fid == 0 {
			return fail(PropertyNotFound)
		}
		return folderPropertyValue{kind: 0x0014, data: binary.LittleEndian.AppendUint64(nil, fid)}
	case 0x7c04: // PR_OST_OSTID: this laboratory store manages no OST value.
		if kind == 0 || kind == 0x0102 {
			return fail(PropertyNotFound)
		}
		return fail(NotSupported)
	default:
		switch tag {
		case 0x35ff0102, 0x66720102, 0x36e5001f, 0x36e6001f, 0x36160102, 0x36d60102, 0x36df0102,
			0x36e00102, 0x36e10003, 0x36e41102, 0x36eb0102, 0x30180102, 0x301e0003:
			return fail(PropertyNotFound)
		}
		// In particular, MS-OXCPRPT 2.2.1.7 explicitly excludes Folder and
		// Logon from PidTagObjectType. Do not invent a MAPI_FOLDER value.
		return fail(NotSupported)
	}
}

// The access mask describes operations available in the authenticated folder
// projection. Native and virtual folders have different write paths.
func (t *SessionTransport) folderAccessProperty(ctx context.Context, box core.Mailbox, folder storeFolder, class string) folderPropertyValue {
	if err := hierarchyOwner(ctx, box); err != nil {
		return folderPropertyValue{kind: 0x000a, code: storeError(err)}
	}
	const (
		modify          = 0x01
		read            = 0x02
		remove          = 0x04
		createHierarchy = 0x08
		createNormal    = 0x10
		createFAI       = 0x20
	)
	if rights, shared := folder.properties[core.SharedFolderRights].(core.Integer); shared {
		access := uint32(read)
		if class == "IPF.Contact" && canWritePersonalContacts(box) && core.AccessRights(rights)&core.AccessCreateItems != 0 {
			access |= createNormal
		}
		return folderPropertyValue{kind: 3, data: append32(nil, access)}
	}
	var access uint32 = read
	if t.folderMetadataSpike || t.ostPropertySpike || t.createFolderSpike && folder.search {
		access |= modify
	}
	if t.createFolderSpike && folder.dynamic {
		access |= remove
	}
	if t.createFolderSpike && !folder.search {
		access |= createHierarchy
	}
	if t.faiCollectorSpike && !folder.search {
		access |= createFAI
	}
	if folder.role == freeBusyFolderRole {
		access |= createNormal
	}
	if folder.role == Outbox && t.canComposeMail(box) {
		access |= createNormal
	}
	if t.contactsMetadataSpike && folder.native != "" && class == "IPF.Contact" && canWritePersonalContacts(box) {
		access |= createNormal
	}
	if t.contentsDownloadSpike && folder.native != "" && (class == "" || class == "IPF.Note") {
		_, canPlace := box.(core.MailMessagePlacementWriter)
		_, canRecover := box.(core.RecoverableMailWriter)
		if canPlace || canRecover {
			access |= createNormal
		}
	}
	return folderPropertyValue{kind: 3, data: append32(nil, access)}
}

func (t *SessionTransport) canComposeMail(box core.Mailbox) bool {
	_, writer := box.(core.MailDraftWriter)
	_, finder := box.(core.MailDraftReferenceFinder)
	_, reader := box.(core.MailMessageMIMEReader)
	_, contents := box.(core.MailContentReader)
	return t.mailDraftCreator != nil && t.mailDraftSender != nil && writer && finder && reader && contents
}

func folderPropertyRow(request specificProperties, values []folderPropertyValue) []byte {
	flagged := false
	for i, value := range values {
		flagged = flagged || value.code != 0 || uint16(request.tags[i]) == 0
	}
	out := []byte{0}
	if flagged {
		out[0] = 1
	}
	for i, value := range values {
		if flagged {
			if uint16(request.tags[i]) == 0 {
				out = binary.LittleEndian.AppendUint16(out, value.kind)
			}
			if value.code != 0 {
				out = append32(append(out, 0x0a), value.code)
				continue
			}
			out = append(out, 0)
		}
		out = append(out, value.data...)
	}
	return out
}

func (t *SessionTransport) getStoreProperties(ctx context.Context, session core.Session, state *contextState, op Operation, handles []uint32, diagnostic *StorePropertyDiagnostic, ost *ostBatchView, metadata *metadataBatchView) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := state.mailbox.lookup(handles[op.Input], op.Logon)
	if op.Properties != nil {
		diagnostic.request(op.Properties.tags)
	}
	if code != 0 {
		return fail(code)
	}
	if object.attachment != nil {
		return getOpenedAttachmentProperties(ctx, session, object, op, diagnostic)
	}
	if object.download != nil {
		if imported := object.download.mailImport; imported != nil && imported.saved {
			// The read uses the published message's scope and revision. The
			// import handle remains available for an idempotent save retry.
			object.message = &openedMessage{
				mid: folderID(imported.counter), codePage: state.codePage,
				contentRevision: imported.contentRevision, contentVersioned: imported.contentVersioned,
			}
		}
	}
	if op.ID == 0x09 || op.ID == ropGetPropertiesAll {
		tags := folderPropertyTags
		switch {
		case object.isLogon:
			tags = logonPropertyTags
		case object.message != nil:
			tags = messagePropertyTags
		case object.download != nil:
			tags = faiPropertyTags
		default:
			tags = slices.Clone(folderPropertyTags)
			for role := metadataFolderRole(0); role < metadataFolderRoleCount; role++ {
				if object.binding.store.metadataRoleEnabled(role) {
					tags = append(tags, role.tag())
				}
			}
		}
		if op.ID == ropGetPropertiesAll {
			request := *op.Properties
			if !object.isLogon && object.message == nil && object.download == nil {
				tags = append(tags, folderRightsTag, folderFlagsTag)
			}
			request.tags = enumeratedPropertyTags(tags, request.wantUnicode)
			request.variableBudget = op.PropertyListLimit
			op.Properties = &request
		} else {
			op.Properties = &specificProperties{wantUnicode: true, tags: tags}
		}
	}
	if object.message != nil {
		return getOpenedMessageProperties(ctx, session, object, op, diagnostic)
	}
	if object.table != nil || object.stream != nil || object.notification != nil {
		return fail(NotSupported)
	}
	if object.download != nil {
		if object.download.draft != nil {
			return getFAIProperties(ctx, session, object.download.draft, op, diagnostic)
		}
		return fail(NotSupported)
	}
	if object.isLogon {
		diagnostic.Logon = true
		return t.getLogonProperties(ctx, session, state, object, op, diagnostic)
	}
	diagnostic.Folder = true
	request := *op.Properties
	folder, err := object.binding.store.folder(ctx, session, object.fid)
	if err != nil {
		return fail(storeError(err))
	}
	if ctx.Err() != nil {
		return fail(0x80004005)
	}
	for _, tag := range request.tags {
		if t.createFolderSpike && (uint16(tag>>16) == 0x6638 || uint16(tag>>16) == 0x360a) {
			count, err := object.binding.store.folderChildCount(ctx, session, object.fid)
			if err != nil {
				return fail(storeError(err))
			}
			folder.childCount, folder.childrenKnown = count, true
			break
		}
	}
	shared := folder.properties[core.SharedFolderRights] != nil
	countRequested := slices.Contains(request.tags, uint32(0x36020003)) || slices.Contains(request.tags, uint32(0x36020000))
	if t.folderAggregates && (slices.ContainsFunc(request.tags, isFolderAggregateTag) || shared && countRequested) {
		folder.aggregate, err = object.binding.store.observeFolderAggregates(ctx, session, folder.native)
		if err != nil {
			folder.aggregateCode = storeError(err)
		} else if shared {
			folder.properties[core.FolderItemCount] = core.Integer(folder.aggregate.count)
		}
	}
	values := make([]folderPropertyValue, len(request.tags))
	service, _ := serviceFolderForRole(folder.role)
	class := service.class
	if class == "" {
		class = nativeFolderClass(folder.properties, object.binding.store.draftsReference)
	}
	composeFolder := t.canComposeMail(session.Mailbox()) &&
		(folder.role == Outbox || object.fid == object.binding.fids[Inbox] || folder.properties[core.WellKnown] == core.Text("drafts"))
	if composeFolder {
		class = "IPF.Note"
	}
	var access folderPropertyValue
	if slices.Contains(request.tags, folderAccessTag) || slices.Contains(request.tags, folderAccessTag&0xffff0000) ||
		composeFolder && (slices.Contains(request.tags, folderRightsTag) || slices.Contains(request.tags, folderRightsTag&0xffff0000)) {
		access = t.folderAccessProperty(ctx, session.Mailbox(), folder, class)
	}
	var rules folderPropertyValue
	if slices.Contains(request.tags, folderHasRules) || slices.Contains(request.tags, folderHasRules&0xffff0000) {
		present, err := inboxHasRules(ctx, session, object.binding, object.fid)
		if err == nil {
			rules = folderPropertyValue{kind: 0x000b, data: []byte{present}}
		} else {
			rules = folderPropertyValue{kind: 0x000a, code: storeError(err)}
		}
	}
	for i, tag := range request.tags {
		if tag == folderAccessTag || tag == folderAccessTag&0xffff0000 {
			values[i] = access
		} else {
			values[i] = projectFolderPropertyWithClass(folder, tag, request.wantUnicode, state.codePage, class)
		}
		switch tag {
		case folderHasRules, folderHasRules & 0xffff0000:
			values[i] = rules
		case folderRightsTag, folderRightsTag & 0xffff0000:
			if composeFolder {
				values[i] = access
				if access.code == 0 {
					values[i].data = append32(nil, ownerFolderRights)
				}
			}
		case folderFlagsTag, folderFlagsTag & 0xffff0000:
			if composeFolder {
				values[i] = folderPropertyValue{kind: 3, data: append32(nil, ipmNormalFolderFlags)}
			}
		case freeBusyEntryIDsTag, freeBusyEntryIDsTag & 0xffff0000:
			values[i] = freeBusyEntryIDs(ctx, session, object)
		case folderEntryIDTag, folderEntryIDTag & 0xffff0000:
			entry := marshalFolderEntryID(object.binding.mailboxGUID, object.binding.store.replica, folder.fid)
			values[i] = folderPropertyValue{kind: 0x0102, data: append([]byte{folderEntryIDBytes, 0}, entry...)}
		case parentFolderEntryIDTag, parentFolderEntryIDTag & 0xffff0000:
			values[i] = folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
			if folder.parent != 0 {
				entry := marshalFolderEntryID(object.binding.mailboxGUID, object.binding.store.replica, folder.parent)
				values[i] = folderPropertyValue{kind: 0x0102, data: append([]byte{folderEntryIDBytes, 0}, entry...)}
			}
		}
		for role := metadataFolderRole(0); role < metadataFolderRoleCount; role++ {
			if object.binding.store.metadataRoleEnabled(role) && uint16(tag>>16) == uint16(role.tag()>>16) {
				values[i] = specialFolderReference(ctx, session, object, tag, role)
			}
		}
		if metadata != nil && metadataID(uint16(tag>>16)) && !(uint16(tag>>16) == 0x3613 && class != "") {
			fid := object.fid
			if inboxMetadataID(uint16(tag>>16)) && fid == object.binding.fids[MailboxRoot] {
				fid = object.binding.fids[Inbox]
			}
			values[i] = metadata.property(fid, tag, request.wantUnicode, state.codePage)
			if uint16(tag>>16) == additionalEntryIDs {
				if values[i].code == PropertyNotFound && fid == object.binding.fids[MailboxRoot] {
					values[i] = metadata.property(object.binding.fids[Inbox], tag, request.wantUnicode, state.codePage)
				}
				values[i] = additionalFolderReferences(ctx, session, object, values[i])
			}
		}
		if ost != nil && uint16(tag>>16) == 0x7c04 && (uint16(tag) == 0 || uint16(tag) == 0x0102) {
			if value, exists := ost.value(object.binding.store, object.fid); exists {
				values[i] = folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(value))), value...)}
			}
		}
		if values[i].code != 0 {
			diagnostic.Errors++
		} else {
			diagnostic.Values++
		}
	}
	return propertyReadResponse(op, values)
}
