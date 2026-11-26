// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mapi contains the initial, explicitly incomplete MAPI/HTTP wire codec.
// Implemented independently from MS-OXCMAPIHTTP, MS-OXCRPC and MS-OXCROPS.
package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

var ErrMalformed = errors.New("malformed MAPI buffer")
var ErrUnsupported = errors.New("MAPI feature outside the initial profile")

// The public opcode label is separate from the error text and request body.
type unsupportedOperation struct {
	label                  string
	synchronization        SynchronizationDiagnostic
	synchronizationOptions SynchronizationOptionsDiagnostic
	createFolder           CreateFolderDiagnostic
	setList                SetListDiagnostic
	tableFlags             rejectedTableFlags
	tableProfile           tableProfileRejection
}

func (unsupportedOperation) Error() string { return ErrUnsupported.Error() }
func (unsupportedOperation) Unwrap() error { return ErrUnsupported }

type malformedOperation struct {
	label, reason string
}

func (malformedOperation) Error() string { return ErrMalformed.Error() }
func (malformedOperation) Unwrap() error { return ErrMalformed }

const MaxBody = 1 << 20
const MaxAuxiliary = 64 << 10
const MaxHandles = 256
const NotSupported uint32 = 0x80040102
const ropNotImplemented uint32 = 0x80040fff
const (
	ropSetProperties            byte = 0x0a
	ropOptionsData              byte = 0x6f
	ropSetPropertiesNoReplicate byte = 0x79
	ropTellVersion              byte = 0x86
	ropProgress                 byte = 0x50
	ropOpenEmbeddedMessage      byte = 0x46
	ropUpdateDeferredActions    byte = 0x57
)

func isSetPropertiesROP(id byte) bool {
	return id == ropSetProperties || id == ropSetPropertiesNoReplicate
}

type reader struct {
	data []byte
	pos  int
	err  error
}

func (r *reader) take(n int) []byte {
	if n < 0 || n > len(r.data)-r.pos {
		r.err = ErrMalformed
		return nil
	}
	v := r.data[r.pos : r.pos+n]
	r.pos += n
	return v
}
func (r *reader) u32() uint32 {
	b := r.take(4)
	if len(b) != 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}
func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *reader) blob(limit int) []byte {
	size := r.u32()
	if size > uint32(limit) {
		r.err = ErrMalformed
		return nil
	}
	return r.take(int(size))
}
func (r *reader) done() error {
	if r.err != nil || r.pos != len(r.data) {
		return ErrMalformed
	}
	return nil
}
func append32(out []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(out, v) }
func appendBlob(out, data []byte) []byte   { return append(append32(out, uint32(len(data))), data...) }

type Connect struct {
	UserDN                                string
	Flags, CodePage, SortLCID, StringLCID uint32
	Auxiliary                             []byte
}

func ParseConnect(data []byte) (Connect, error) {
	if len(data) > MaxBody {
		return Connect{}, ErrMalformed
	}
	r := reader{data: data}
	end := bytes.IndexByte(data, 0)
	if end < 1 || end > 4096 {
		return Connect{}, ErrMalformed
	}
	for _, b := range data[:end] {
		if b > 127 || b < 32 {
			return Connect{}, ErrMalformed
		}
	}
	dn := string(r.take(end + 1)[:end])
	v := Connect{UserDN: dn, Flags: r.u32(), CodePage: r.u32(), SortLCID: r.u32(), StringLCID: r.u32()}
	v.Auxiliary = bytes.Clone(r.blob(MaxAuxiliary))
	return v, r.done()
}
func (v Connect) Marshal() ([]byte, error) {
	if len(v.UserDN) < 1 || len(v.UserDN) > 4096 || len(v.Auxiliary) > MaxAuxiliary {
		return nil, ErrMalformed
	}
	for _, b := range []byte(v.UserDN) {
		if b < 32 || b > 127 {
			return nil, ErrMalformed
		}
	}
	out := append([]byte(v.UserDN), 0)
	for _, n := range []uint32{v.Flags, v.CodePage, v.SortLCID, v.StringLCID} {
		out = append32(out, n)
	}
	return appendBlob(out, v.Auxiliary), nil
}

type Execute struct {
	Flags     uint32
	ROP       []byte
	MaxOutput uint32
	Auxiliary []byte
}

func ParseExecute(data []byte) (Execute, error) {
	if len(data) > MaxBody {
		return Execute{}, ErrMalformed
	}
	r := reader{data: data}
	v := Execute{Flags: r.u32()}
	v.ROP = bytes.Clone(r.blob(MaxBody))
	v.MaxOutput = r.u32()
	v.Auxiliary = bytes.Clone(r.blob(MaxAuxiliary))
	if err := r.done(); err != nil {
		return Execute{}, err
	}
	if v.MaxOutput > MaxBody {
		return Execute{}, ErrMalformed
	}
	return v, nil
}
func (v Execute) Marshal() ([]byte, error) {
	if len(v.ROP) > MaxBody || len(v.Auxiliary) > MaxAuxiliary || v.MaxOutput > MaxBody {
		return nil, ErrMalformed
	}
	out := appendBlob(append32(nil, v.Flags), v.ROP)
	out = appendBlob(append32(out, v.MaxOutput), v.Auxiliary)
	if len(out) > MaxBody {
		return nil, ErrMalformed
	}
	return out, nil
}

// ROPBuffer covers a single uncompressed, unobfuscated Last extended buffer.
// Unknown ROPs retain their framing but are not claimed to have valid semantics.
type ROPBuffer struct {
	Operations []byte
	Handles    []uint32
}

func ParseROP(data []byte) (ROPBuffer, error) {
	operations, handles, err := plainROPFields(data)
	if err != nil {
		return ROPBuffer{}, err
	}
	out := ROPBuffer{Operations: bytes.Clone(operations)}
	for at := 0; at < len(handles); at += 4 {
		out.Handles = append(out.Handles, binary.LittleEndian.Uint32(handles[at:]))
	}
	return out, nil
}

// Borrow the framing fields without copying opaque operation payloads. Callers
// must not retain these slices; ParseROP returns its usual detached snapshot.
func plainROPFields(data []byte) (operations, handles []byte, err error) {
	if len(data) < 10 || len(data) > MaxBody {
		return nil, nil, ErrMalformed
	}
	version := binary.LittleEndian.Uint16(data)
	flags := binary.LittleEndian.Uint16(data[2:])
	size := int(binary.LittleEndian.Uint16(data[4:]))
	actual := int(binary.LittleEndian.Uint16(data[6:]))
	if version != 0 || flags & ^uint16(7) != 0 || size != len(data)-8 {
		return nil, nil, ErrMalformed
	}
	if flags&1 == 0 && size != actual || flags&1 != 0 && size >= actual {
		return nil, nil, ErrMalformed
	}
	if flags != 4 {
		return nil, nil, ErrUnsupported
	}
	payload := data[8:]
	ropSize := int(binary.LittleEndian.Uint16(payload))
	if ropSize < 2 || ropSize > len(payload) || (len(payload)-ropSize)%4 != 0 || (len(payload)-ropSize)/4 > MaxHandles {
		return nil, nil, ErrMalformed
	}
	return payload[2:ropSize], payload[ropSize:], nil
}
func (v ROPBuffer) Marshal() ([]byte, error) {
	size := 2 + len(v.Operations) + 4*len(v.Handles)
	if size > 65535 || len(v.Handles) > MaxHandles {
		return nil, ErrMalformed
	}
	out := binary.LittleEndian.AppendUint16(nil, 0)
	out = binary.LittleEndian.AppendUint16(out, 4)
	out = binary.LittleEndian.AppendUint16(out, uint16(size))
	out = binary.LittleEndian.AppendUint16(out, uint16(size))
	out = binary.LittleEndian.AppendUint16(out, uint16(2+len(v.Operations)))
	out = append(out, v.Operations...)
	for _, h := range v.Handles {
		out = append32(out, h)
	}
	return out, nil
}

type Operation struct {
	Notification                *notificationRequest
	Table                       *tableRequest
	Stream                      streamRequest
	TransferCopy                fastTransferCopyRequest
	TransferMessages            fastTransferCopyMessagesRequest
	TransferSourceOperation     byte
	TransferData                []byte
	CopyMessageTo               copyMessageToRequest
	OpenMessage                 *openMessageRequest
	CreateMessageCodePage       uint16
	ReadRecipients              *readRecipientsRequest
	ModifyRecipients            []byte
	MoveCopyMessages            moveCopyMessagesRequest
	FolderPlacement             folderPlacementRequest
	ImportMessageMove           importMessageMoveRequest
	DeleteMessages              deleteMessagesRequest
	EmptyFolder                 emptyFolderRequest
	OpenAttachment              *openAttachmentRequest
	DeleteAttachment            uint32
	AttachmentLimit             int
	PropertyListLimit           int
	NamedPropertyIDs            []uint16
	NamedPropertySet            *[16]byte
	FAIImport                   *faiImportRequest
	FAISet                      faiSetRequest
	MailDraftSet                bool
	ContactSet                  bool
	AppointmentSet              bool
	ImportMessageSet            bool
	DeletePropertyTags          []uint32
	FAIDeletes                  *faiDeleteRequest
	ReadStates                  []readStateChange
	ReadMIDs                    []uint64
	MessageStatusMID            uint64
	MessageStatusFlags          uint32
	MessageStatusMask           uint32
	SubmissionMID               uint64
	TransportNewMail            transportNewMailRequest
	ID, Logon, Input, Output    byte
	Folder                      uint64
	LongTermID                  [24]byte
	PerUser                     perUserRequest
	ReceiveTableLimit           int
	SpecialFolderReference      *specialFolderReferenceSet
	FreeBusyReference           *freeBusyReferenceSet
	Flags                       byte
	LogonRequest                *LogonRequest
	PropertyNames               []propertyName
	Properties                  *specificProperties
	PermissionChange            permissionRequest
	RuleChanges                 []byte
	DeferredAction              deferredActionRequest
	OSTValue                    []byte
	Metadata                    folderMetadata
	ReplicaIDCount              uint32
	LocalReplicaDeleted         []localReplicaRange
	MessageClass                string
	InvalidMessageClass         bool
	CreateFolder                *createFolderRequest
	Search                      searchCriteriaOperation
	Configure                   *synchronizationConfigure
	Contents                    *contentsConfigure
	Upload                      *hierarchyUpload
	HierarchyImport             *hierarchyImport
	TransferSize, TransferLimit uint16
}

// DecodeOperations validates the entire supported store-operation
// batch before returning any operation. It never allocates or executes objects.
func (v ROPBuffer) DecodeOperations() ([]Operation, error) {
	return v.decodeOperations(false)
}

func (v ROPBuffer) decodeOperations(ost bool) ([]Operation, error) {
	return v.decodeStoreOperations(ost, false)
}

func (v ROPBuffer) decodeStoreOperations(ost, hierarchy bool) ([]Operation, error) {
	return v.decodeLaboratoryOperations(ost, hierarchy, false)
}

func (v ROPBuffer) decodeLaboratoryOperations(ost, hierarchy, create bool) ([]Operation, error) {
	return v.decodeFolderOperations(ost, hierarchy, create, false)
}

func (v ROPBuffer) decodeFolderOperations(ost, hierarchy, create, metadata bool) ([]Operation, error) {
	return v.decodeTracedFolderOperations(ost, hierarchy, create, metadata, false)
}

func (v ROPBuffer) decodeTracedFolderOperations(ost, hierarchy, create, metadata, setListTrace bool) ([]Operation, error) {
	return v.decodeCalendarFolderOperations(ost, hierarchy, create, metadata, setListTrace, false)
}

func (v ROPBuffer) decodeCalendarFolderOperations(ost, hierarchy, create, metadata, setListTrace, calendar bool) ([]Operation, error) {
	return v.decodeContentsOperations(ost, hierarchy, create, metadata, setListTrace, calendar, false)
}

func (v ROPBuffer) decodeContentsOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contents bool) ([]Operation, error) {
	return v.decodeFAIOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contents, false)
}
func (v ROPBuffer) decodeFAIOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contents, collector bool) ([]Operation, error) {
	return v.decodeSpecialFolderOperations(ost, hierarchy, create, metadata, setListTrace, calendar, false, false, contents, collector)
}
func (v ROPBuffer) decodeSpecialFolderOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, contents, collector bool) ([]Operation, error) {
	return v.decodeReferenceFolderOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, false, contents, collector)
}
func (v ROPBuffer) decodeReferenceFolderOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, drafts, contents, collector bool) (operations []Operation, decodeErr error) {
	return v.decodeTableOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, drafts, contents, collector, false)
}

func (v ROPBuffer) decodeTableOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, drafts, contents, collector, tables bool) (operations []Operation, decodeErr error) {
	return v.decodeReadStateOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, drafts, contents, collector, tables, false, false, false, nil, nil)
}

func (v ROPBuffer) decodeReadStateOperations(ost, hierarchy, create, metadata, setListTrace, calendar, contacts, tasks, drafts, contents, collector, tables, readState, ramStore, privateConfig bool, mailDraftObjects, importObjects *mailboxObjects) (operations []Operation, decodeErr error) {
	attachmentObjects := mailDraftObjects
	if attachmentObjects == nil {
		attachmentObjects = importObjects
	}
	currentLabel := "other"
	reason := "invalid_fields"
	defer func() {
		var existing unsupportedOperation
		if errors.Is(decodeErr, ErrUnsupported) && !errors.As(decodeErr, &existing) {
			decodeErr = unsupportedOperation{label: currentLabel}
		}
		if ramStore && errors.Is(decodeErr, ErrMalformed) {
			decodeErr = malformedOperation{label: currentLabel, reason: reason}
		}
	}()
	if len(v.Handles) > MaxHandles || 2+len(v.Operations)+4*len(v.Handles) > 65535 {
		return nil, ErrMalformed
	}
	r := reader{data: v.Operations}
	var out []Operation
	provisionalDrafts := make(map[byte]byte)
	provisionalContacts := make(map[byte]byte)
	provisionalAppointments := make(map[byte]byte)
	type pendingMessageImport struct {
		logon      byte
		associated bool
	}
	provisionalImports := make(map[byte]pendingMessageImport)
	var publicLogons [256]bool
	if importObjects != nil {
		for logon, handle := range importObjects.logons {
			publicLogons[logon] = importObjects.objects[handle].public != nil
		}
	}
	for r.pos < len(r.data) {
		currentLabel, reason = "other", "invalid_fields"
		if len(out) >= 1024 {
			reason = "operation_count"
			return nil, ErrMalformed
		}
		start := r.pos
		header := r.take(3)
		if r.err != nil {
			reason = "truncated_operation_header"
			return nil, r.err
		}
		op := Operation{ID: header[0], Logon: header[1], Input: header[2]}
		currentLabel = publicROPLabels[op.ID]
		if currentLabel == "" {
			currentLabel = "other"
		}
		if int(header[2]) >= len(v.Handles) {
			reason = "handle_index"
			return nil, ErrMalformed
		}
		switch op.ID {
		case 0x29:
			if err := parseNotificationRegistration(&r, &op, len(v.Handles)); err != nil {
				return nil, err
			}
		case 0x03:
			if !contents {
				return nil, ErrUnsupported
			}
			rest := r.take(20)
			if r.err != nil {
				return nil, r.err
			}
			op.Output = rest[0]
			if int(op.Output) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Folder = binary.LittleEndian.Uint64(rest[3:11])
			op.OpenMessage = &openMessageRequest{codePage: binary.LittleEndian.Uint16(rest[1:3]), mode: rest[11], mid: binary.LittleEndian.Uint64(rest[12:])}
			delete(provisionalContacts, op.Output)
			delete(provisionalAppointments, op.Output)
			delete(provisionalDrafts, op.Output)
			if (contacts || calendar) && collector && importObjects != nil && op.OpenMessage.mode&3 != 0 {
				object, code := importObjects.lookup(v.Handles[op.Input], op.Logon)
				if code == 0 && object.binding.store != nil {
					if contacts && object.binding.store.isContactMID(op.OpenMessage.mid) {
						provisionalContacts[op.Output] = op.Logon
					}
					if calendar && object.binding.store.isAppointmentMID(op.OpenMessage.mid) {
						provisionalAppointments[op.Output] = op.Logon
					}
				}
			}
		case ropReloadCachedInformation:
			if !contents {
				return nil, ErrUnsupported
			}
			r.take(2) // Reserved. The server ignores this field.
			op.OpenMessage = &openMessageRequest{}
		case ropOpenEmbeddedMessage:
			fields := r.take(4)
			if r.err != nil || int(fields[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Output = fields[0]
			op.OpenMessage = &openMessageRequest{codePage: binary.LittleEndian.Uint16(fields[1:3]), mode: fields[3]}
		case ropGetMessageStatus, ropSetMessageStatus:
			if !contents && op.ID == ropGetMessageStatus {
				return nil, ErrUnsupported
			}
			raw := r.take(8)
			if r.err != nil {
				return nil, r.err
			}
			op.MessageStatusMID = binary.LittleEndian.Uint64(raw)
			if op.ID == ropSetMessageStatus {
				flags := r.take(8)
				if r.err != nil {
					return nil, r.err
				}
				op.MessageStatusFlags = binary.LittleEndian.Uint32(flags)
				op.MessageStatusMask = binary.LittleEndian.Uint32(flags[4:])
			}
		case 0x06:
			if !collector {
				return nil, ErrUnsupported
			}
			rest := r.take(12)
			if r.err != nil {
				return nil, r.err
			}
			op.Output = rest[0]
			if int(op.Output) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.CreateMessageCodePage = binary.LittleEndian.Uint16(rest[1:3])
			op.Folder = binary.LittleEndian.Uint64(rest[3:11])
			op.Flags = rest[11]
			delete(provisionalContacts, op.Output)
			delete(provisionalAppointments, op.Output)
			delete(provisionalDrafts, op.Output)
			if op.Flags == 0 && (mailDraftObjects != nil || contacts && importObjects != nil) {
				objects := importObjects
				if objects == nil {
					objects = mailDraftObjects
				}
				logon, code := objects.logonObject(objects.logons[op.Logon], op.Logon)
				if code == 0 && !logon.binding.store.isFreeBusyFolder(op.Folder) {
					provisionalDrafts[op.Output] = op.Logon
				}
			}
		case 0x2b, 0x2c, 0x2e, 0x5e, ropWriteStream, ropSetStreamSize, ropCommitStream, ropCopyToStream,
			ropCloneStream, ropWriteAndCommitStream, ropLockRegionStream, ropUnlockRegionStream:
			if !contents {
				return nil, ErrUnsupported
			}
			if err := parseAttachmentStream(&r, &op, len(v.Handles)); err != nil {
				return nil, err
			}
		case 0x22:
			if !contents {
				return nil, ErrUnsupported
			}
			rest := r.take(6)
			if r.err != nil {
				return nil, r.err
			}
			op.Output = rest[0]
			if int(op.Output) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.OpenAttachment = &openAttachmentRequest{mode: rest[1], number: binary.LittleEndian.Uint32(rest[2:])}
		case ropCreateAttachment:
			b := r.take(1)
			if r.err != nil || int(b[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Output = b[0]
			if attachmentObjects != nil {
				provisionalDrafts[op.Output] = op.Logon
			}
		case ropSaveAttachment:
			b := r.take(2)
			if r.err != nil || int(b[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Input, op.Output, op.Flags = b[0], header[2], b[1]
		case ropDeleteAttachment:
			if !contents {
				return nil, ErrUnsupported
			}
			op.DeleteAttachment = r.u32()
		case 0x52:
			if !contents {
				return nil, ErrUnsupported
			}
		case ropSetMessageReadFlag:
			if !contents {
				return nil, ErrUnsupported
			}
			rest := r.take(2)
			if r.err != nil || int(rest[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Input, op.Output, op.Flags = rest[0], header[2], rest[1]
		case ropSetReadFlags:
			if !contents {
				return nil, ErrUnsupported
			}
			if err := parseSetReadFlags(&r, &op); err != nil {
				return nil, err
			}
		case ropDeleteMessages, ropHardDeleteMessages:
			if op.ID == ropDeleteMessages && !contents {
				return nil, ErrUnsupported
			}
			if err := parseDeleteMessages(&r, &op.DeleteMessages); err != nil {
				return nil, err
			}
		case ropEmptyFolder, ropHardDeleteFolderContents:
			flags := r.take(2)
			if r.err != nil {
				return nil, r.err
			}
			op.EmptyFolder = emptyFolderRequest{async: flags[0] != 0, deleteAssociated: flags[1] != 0}
		case ropMoveCopyMessages:
			if !contents {
				return nil, ErrUnsupported
			}
			if err := parseMoveCopyMessages(&r, &op.MoveCopyMessages, len(v.Handles)); err != nil {
				return nil, err
			}
		case ropMoveFolder, ropCopyFolder:
			request, err := parseFolderPlacement(&r, len(v.Handles), op.ID)
			if err != nil {
				return nil, err
			}
			op.FolderPlacement = request
		case ropCopyTo, ropCopyProperties:
			if mailDraftObjects == nil {
				return nil, ErrUnsupported
			}
			if err := parseCopyMessageTo(&r, &op.CopyMessageTo, len(v.Handles), op.ID == ropCopyProperties); err != nil {
				return nil, err
			}
		case 0x0d, 0x0e:
			if !contents {
				return nil, ErrUnsupported
			}
			if op.ID == 0x0d {
				r.take(4) // Reserved.
			} else {
				start := r.pos
				skipModifyRecipients(&r)
				op.ModifyRecipients = r.data[start:r.pos]
			}
			if r.err != nil {
				return nil, r.err
			}
		case 0x0f:
			if !contents {
				return nil, ErrUnsupported
			}
			rest := r.take(6)
			if r.err != nil {
				return nil, r.err
			}
			if binary.LittleEndian.Uint16(rest[4:]) != 0 {
				return nil, ErrMalformed
			}
			op.ReadRecipients = &readRecipientsRequest{rowID: binary.LittleEndian.Uint32(rest)}
		case 0x04, 0x05, 0x21, 0x3e, 0x3f, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x19, 0x1a, 0x1b, 0x38, 0x4f, 0x59, 0x5a, 0x6b, 0x6c, 0x81, 0x89:
			if !tables || op.ID == 0x21 && !contents {
				return nil, ErrUnsupported
			}
			if err := parseTableOperation(&r, &op, len(v.Handles)); err != nil {
				if errors.Is(err, ErrUnsupported) && (op.ID == 4 || op.ID == 5 || op.ID == 0x21 || op.ID == 0x3e) {
					return nil, unsupportedOperation{label: currentLabel, tableFlags: rejectedTableFlags{op.ID, op.Table.flags}}
				}
				if errors.Is(err, ErrUnsupported) {
					return nil, unsupportedOperation{label: currentLabel, tableProfile: op.Table.rejection}
				}
				return nil, err
			}
		case 0x40:
			if !tables {
				return nil, ErrUnsupported
			}
			if err := parsePermissionRequest(&r, &op.PermissionChange); err != nil {
				return nil, err
			}
		case ropGetOwningServers, ropPublicFolderIsGhosted:
			op.Folder = r.u64()
		case ropGetPerUserLongTermIDs:
			copy(op.PerUser.replica[:], r.take(16))
		case ropGetPerUserGUID:
			copy(op.LongTermID[:], r.take(24))
		case ropReadPerUserInformation, ropWritePerUserInformation:
			readPerUserRequest(&r, &op, publicLogons[op.Logon])
		case 0x43, 0x44:
			if !ramStore && !calendar && !contacts && !tasks && !drafts {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			if op.ID == 0x43 {
				b := r.take(8)
				if r.err != nil {
					return nil, r.err
				}
				op.Folder = binary.LittleEndian.Uint64(b)
			} else {
				b := r.take(24)
				if r.err != nil {
					return nil, r.err
				}
				copy(op.LongTermID[:], b)
			}
		case 1: // Release has no corresponding ROP response.
			delete(provisionalDrafts, op.Input)
			delete(provisionalContacts, op.Input)
			delete(provisionalAppointments, op.Input)
			delete(provisionalImports, op.Input)
		case ropSubmitMessage:
			flags := r.take(1)
			if r.err != nil {
				return nil, r.err
			}
			op.Flags = flags[0]
		case ropAbortSubmit:
			op.Folder, op.SubmissionMID = r.u64(), r.u64()
		case ropSpoolerLockMessage:
			op.SubmissionMID = r.u64()
			if b := r.take(1); b != nil {
				op.Flags = b[0]
			}
		case ropModifyRules:
			start := r.pos
			if err := skipModifyRules(&r); err != nil {
				return nil, err
			}
			op.RuleChanges = r.data[start:r.pos]
		case ropUpdateDeferredActions:
			for _, field := range []*[]byte{&op.DeferredAction.server, &op.DeferredAction.client} {
				size := r.take(2)
				if r.err != nil {
					return nil, r.err
				}
				*field = r.take(int(binary.LittleEndian.Uint16(size)))
			}
		case ropTransportNewMail:
			ids := r.take(16)
			if r.err != nil {
				return nil, r.err
			}
			class := recipientASCII(&r)
			flags := r.take(4)
			if r.err != nil {
				return nil, r.err
			}
			op.TransportNewMail = transportNewMailRequest{
				mid: binary.LittleEndian.Uint64(ids), fid: binary.LittleEndian.Uint64(ids[8:]),
				class: class, flags: binary.LittleEndian.Uint32(flags),
			}
		case ropTransportSend, ropGetTransportFolder, ropSetSpooler: // Only the common input-handle header.
		case 0x49: // GetAddressTypes has only the common three-byte header.
		case ropOptionsData:
			// Consume AddressType and WantWin32. No address type has a help file.
			recipientASCII(&r)
			r.take(1)
			if r.err != nil {
				return nil, r.err
			}
		case 0x09, 0x37, 0x68, ropGetStoreState: // Only the common input-handle header.
		case 0x27, ropSetReceiveFolder:
			if op.ID == ropSetReceiveFolder {
				op.Folder = r.u64()
				if r.err != nil {
					return nil, r.err
				}
			}
			end := bytes.IndexByte(r.data[r.pos:], 0)
			if end < 0 {
				return nil, ErrMalformed
			}
			value := r.take(end + 1)[:end]
			op.InvalidMessageClass = !validReceiveClass(value)
			if !op.InvalidMessageClass {
				op.MessageClass = string(value)
			}
		case 0x1c:
			if !create {
				return nil, unsupportedOperation{label: "RopCreateFolder", createFolder: inspectCreateFolderHeader(r.data[r.pos:], len(v.Handles))}
			}
			request, err := parseCreateFolder(&r, len(v.Handles))
			if err != nil {
				return nil, err
			}
			op.Output, op.CreateFolder = request.output, &request
		case ropDeleteFolder:
			if !create {
				return nil, unsupportedOperation{label: "RopDeleteFolder"}
			}
			if err := parseDeleteFolder(&r, &op); err != nil {
				return nil, err
			}
		case ropSetSearchCriteria, ropGetSearchCriteria:
			if !create {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			if err := parseSearchCriteriaOperation(&r, &op); err != nil {
				return nil, err
			}
		case 0x70:
			diagnostic := inspectSynchronizationHeader(r.data[r.pos:], len(v.Handles))
			if !hierarchy {
				return nil, unsupportedOperation{label: "RopSynchronizationConfigure", synchronization: diagnostic}
			}
			if contents && len(r.data)-r.pos >= 2 && r.data[r.pos+1] == 1 {
				config, options, err := parseContentsConfigureDiagnostic(&r, len(v.Handles))
				if err != nil {
					if errors.Is(err, ErrUnsupported) {
						return nil, unsupportedOperation{label: "RopSynchronizationConfigure", synchronization: diagnostic, synchronizationOptions: options}
					}
					return nil, err
				}
				if validateContentsConfigure(config.frame) != nil {
					return nil, unsupportedOperation{label: "RopSynchronizationConfigure", synchronization: diagnostic, synchronizationOptions: options}
				}
				op.Output, op.Configure, op.Contents = config.frame.output, &config.frame, &config
				break
			}
			config, err := parseSynchronizationConfigure(&r, len(v.Handles))
			if err != nil {
				return nil, err
			}
			if validateHierarchyConfigure(config) != nil {
				return nil, unsupportedOperation{label: "RopSynchronizationConfigure", synchronization: diagnostic, synchronizationOptions: inspectSynchronizationOptions(config)}
			}
			op.Output, op.Configure = config.output, &config
		case 0x7e, 0x72, 0x74, 0x0c:
			if !collector {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			switch op.ID {
			case 0x7e:
				output, contents, err := parseCollectorOpen(&r, len(v.Handles), hierarchy)
				if err != nil {
					return nil, err
				}
				op.Output = output
				if contents {
					op.Flags = 1
				}
			case 0x72:
				request, err := parseFAIImport(&r, len(v.Handles))
				if err != nil {
					var rejection importRejection
					if errors.As(err, &rejection) {
						return nil, unsupportedOperation{label: currentLabel, synchronizationOptions: SynchronizationOptionsDiagnostic{importRejection: rejection}}
					}
					return nil, err
				}
				op.Output, op.FAIImport = request.output, &request
				provisionalImports[request.output] = pendingMessageImport{op.Logon, request.flags&0x10 != 0}
			case 0x74:
				request, err := parseFAIDeletes(&r)
				if err != nil {
					var rejection importRejection
					if errors.As(err, &rejection) {
						return nil, unsupportedOperation{label: currentLabel, synchronizationOptions: SynchronizationOptionsDiagnostic{importRejection: rejection}}
					}
					return nil, err
				}
				op.FAIDeletes = &request
			case 0x0c:
				input, flags, err := parseFAISave(&r, len(v.Handles))
				if err != nil {
					return nil, err
				}
				op.Input, op.Output, op.Flags = input, header[2], flags
			}
		case 0x80:
			if !collector || !readState {
				return nil, unsupportedOperation{label: currentLabel}
			}
			changes, err := parseReadStateChanges(&r)
			if err != nil {
				return nil, err
			}
			op.ReadStates = changes
		case 0x73:
			if !hierarchy || !collector {
				return nil, unsupportedOperation{label: currentLabel}
			}
			request, err := parseHierarchyImport(&r)
			if err != nil {
				if errors.Is(err, ErrUnsupported) {
					return nil, unsupportedOperation{label: currentLabel, synchronizationOptions: SynchronizationOptionsDiagnostic{importRejection: importHierarchyUnsupported}}
				}
				return nil, err
			}
			op.HierarchyImport = &request
		case 0x78:
			if err := parseImportMessageMove(&r, &op.ImportMessageMove); err != nil {
				return nil, err
			}
		case 0x75, 0x76, 0x77:
			if !hierarchy {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			upload, err := parseHierarchyUpload(&r, op.ID)
			if err != nil {
				return nil, err
			}
			op.Upload = &upload
		case 0x82:
			if !hierarchy {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			output := r.take(1)
			if r.err != nil || int(output[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Output = output[0]
		case ropFastTransferSourceCopyMessages:
			if !hierarchy || !contents {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			if err := parseFastTransferCopyMessages(&r, &op, len(v.Handles)); err != nil {
				return nil, err
			}
		case ropFastTransferSourceCopyFolder:
			if !hierarchy || !contents {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			fields := r.take(3)
			if r.err != nil || int(fields[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Output, op.Flags, op.TransferCopy.sendOptions = fields[0], fields[1], fields[2]
		case 0x4d, ropFastTransferSourceCopyProperties:
			if !hierarchy || !contents {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			if err := parseFastTransferCopy(&r, &op, len(v.Handles)); err != nil {
				return nil, err
			}
		case ropFastTransferDestinationConfigure:
			fields := r.take(3)
			if r.err != nil || int(fields[0]) >= len(v.Handles) {
				return nil, ErrMalformed
			}
			op.Output, op.TransferSourceOperation, op.Flags = fields[0], fields[1], fields[2]
		case ropFastTransferDestinationPutBuffer:
			size := r.take(2)
			if r.err != nil {
				return nil, ErrMalformed
			}
			op.TransferData = r.take(int(binary.LittleEndian.Uint16(size)))
			if r.err != nil {
				return nil, ErrMalformed
			}
		case ropTellVersion:
			// The peer version is three WORDs. The supported wire profile
			// is version-independent, so no peer state is retained.
			if r.take(6) == nil {
				return nil, ErrMalformed
			}
		case ropProgress:
			cancel := r.take(1)
			if r.err != nil {
				return nil, r.err
			}
			op.Flags = cancel[0]
		case 0x4e:
			if !hierarchy {
				return nil, unsupportedOperation{label: publicROPLabels[op.ID]}
			}
			size := r.take(2)
			if r.err != nil {
				return nil, ErrMalformed
			}
			op.TransferSize = binary.LittleEndian.Uint16(size)
			op.TransferLimit = op.TransferSize
			if op.TransferSize == 0xbabe {
				maximum := r.take(2)
				if r.err != nil {
					return nil, ErrMalformed
				}
				op.TransferLimit = binary.LittleEndian.Uint16(maximum)
			}
		case ropSetLocalReplicaMidsetDeleted:
			ranges, err := decodeLocalReplicaDeletedRanges(&r)
			if err != nil {
				return nil, err
			}
			op.LocalReplicaDeleted = ranges
		case 0x7f:
			count := r.take(4)
			if r.err != nil {
				return nil, r.err
			}
			op.ReplicaIDCount = binary.LittleEndian.Uint32(count)
		case ropDeleteProperties, ropDeletePropertiesNoReplicate:
			if !ramStore {
				return nil, unsupportedOperation{label: currentLabel}
			}
			tags, err := parseDeletePropertyTags(&r)
			if err != nil {
				return nil, err
			}
			op.DeletePropertyTags = tags
		case ropSetProperties, ropSetPropertiesNoReplicate:
			setError := func(err error) error {
				var unsupported unsupportedOperation
				if setListTrace && errors.Is(err, ErrUnsupported) {
					if !errors.As(err, &unsupported) {
						unsupported.label = currentLabel
					}
					if unsupported.label == currentLabel {
						unsupported.setList = inspectSetListOperation(v.Operations[start:], len(v.Handles))
						return unsupported
					}
				}
				return err
			}
			if calendar && collector && importObjects != nil {
				object, code := importObjects.lookup(v.Handles[op.Input], op.Logon)
				logon, provisional := provisionalAppointments[op.Input]
				if provisional && logon == op.Logon || code == 0 && object.message != nil && object.message.appointmentDraft != nil {
					values, err := parseMailImportSet(&r)
					if err != nil {
						return nil, setError(err)
					}
					op.FAISet, op.AppointmentSet = values, true
					break
				}
			}
			if contacts && collector && importObjects != nil {
				object, code := importObjects.lookup(v.Handles[op.Input], op.Logon)
				logon, provisional := provisionalContacts[op.Input]
				createLogon, creating := provisionalDrafts[op.Input]
				creating = creating && createLogon == op.Logon
				if creating || provisional && logon == op.Logon || code == 0 && object.message != nil && object.message.contactDraft != nil {
					values, err := parseMessageSetValues(&r, readContactSetValue)
					if err != nil {
						return nil, setError(err)
					}
					op.FAISet, op.ContactSet = values, true
					op.MailDraftSet = creating
					break
				}
			}
			if collector && importObjects != nil {
				object, code := importObjects.lookup(v.Handles[op.Input], op.Logon)
				pendingImport, importing := provisionalImports[op.Input]
				importing = importing && pendingImport.logon == op.Logon
				if importing && !pendingImport.associated || code == 0 && object.download != nil && object.download.mailImport != nil {
					values, err := parseMailImportSet(&r)
					if err != nil {
						return nil, setError(err)
					}
					op.FAISet, op.ImportMessageSet = values, true
					break
				}
				associated := code == 0 && (object.message != nil && object.message.draft != nil ||
					object.download != nil && object.download.draft != nil)
				if importing && pendingImport.associated || associated {
					values, err := parseAssociatedMessageSet(&r, true)
					if err != nil {
						return nil, setError(err)
					}
					op.FAISet = values
					break
				}
			}
			if attachmentObjects != nil {
				object, code := attachmentObjects.lookup(v.Handles[op.Input], op.Logon)
				provisionalLogon, provisional := provisionalDrafts[op.Input]
				editable := code == 0 && (mailDraftObjects != nil && object.message != nil && object.message.mailDraft != nil ||
					object.attachment != nil && object.attachment.draft != nil)
				if provisional && provisionalLogon == op.Logon || editable {
					values, err := parseMailDraftSet(&r)
					if err != nil {
						return nil, setError(err)
					}
					op.FAISet, op.MailDraftSet = values, true
					break
				}
			}
			if collector && len(r.data)-r.pos >= 8 && storeSetTag(binary.LittleEndian.Uint32(r.data[r.pos+4:])) {
				values, err := parseFAISet(&r)
				if err != nil {
					return nil, setError(err)
				}
				op.FAISet = values
				break
			}
			if privateConfig && len(r.data)-r.pos >= 8 && binary.LittleEndian.Uint32(r.data[r.pos+4:]) == freeBusyEntryIDsTag {
				value, err := parseFreeBusyReferenceSet(&r)
				if err != nil {
					return nil, setError(err)
				}
				op.FreeBusyReference = value
				break
			}
			// Native references permit idempotent reaffirmation. Computed service
			// references are decoded to return their individual property errors.
			enabled := [metadataFolderRoleCount]bool{calendar, contacts, tasks, drafts, ramStore, ramStore}
			isReference := false
			if len(r.data)-r.pos >= 8 {
				tag := binary.LittleEndian.Uint32(r.data[r.pos+4:])
				for role := metadataFolderRole(0); role < metadataFolderRoleCount; role++ {
					isReference = isReference || enabled[role] && tag == role.tag()
				}
				// A Reminders-first list can also be an ordinary metadata write.
				// Only consume it here if the remaining tags are references too.
				if metadata && tag == remindersReferenceTag && binary.LittleEndian.Uint16(r.data[r.pos+2:]) > 1 {
					probe := r
					if value, err := parseSpecialFolderReferenceSet(&probe, enabled, true); err == nil {
						r, op.SpecialFolderReference = probe, value
						break
					}
				}
			}
			if isReference {
				value, err := parseSpecialFolderReferenceSet(&r, enabled, metadata)
				if err != nil {
					return nil, setError(err)
				}
				op.SpecialFolderReference = value
				break
			}
			// OST remains a separate closed contract, not an arbitrary property bag.
			isOST := len(r.data)-r.pos >= 8 && binary.LittleEndian.Uint16(r.data[r.pos+2:]) == 1 && binary.LittleEndian.Uint32(r.data[r.pos+4:]) == ostPropertyTag
			if metadata && !isOST {
				value, err := parseFolderMetadata(&r)
				if err != nil {
					return nil, setError(err)
				}
				op.Metadata = value
				break
			}
			if !ost {
				return nil, setError(unsupportedOperation{label: currentLabel})
			}
			value, err := parseOSTValue(&r)
			if err != nil {
				return nil, setError(err)
			}
			op.OSTValue = value
		case 0x07:
			request, err := parseSpecificProperties(&r)
			if err != nil {
				reason = "truncated_property_request"
				return nil, err
			}
			op.Properties = &request
		case ropGetPropertiesAll:
			wire := r.take(4)
			if r.err != nil {
				return nil, ErrMalformed
			}
			op.Properties = &specificProperties{
				sizeLimit:   binary.LittleEndian.Uint16(wire),
				wantUnicode: binary.LittleEndian.Uint16(wire[2:]) != 0,
			}
		case 0xfe:
			// Logon has an output slot, not an input handle. LogonId is an
			// independent byte chosen by the client, not a table index.
			op.Input, op.Output = 0, header[2]
			request, err := parseLogonRequest(&r)
			if err != nil {
				return nil, err
			}
			op.LogonRequest = &request
			publicLogons[op.Logon] = request.Flags&1 == 0
		case 2:
			rest := r.take(10)
			if r.err != nil {
				return nil, r.err
			}
			op.Output = rest[0]
			op.Folder = binary.LittleEndian.Uint64(rest[1:9])
			op.Flags = rest[9]
			if int(op.Output) >= len(v.Handles) {
				return nil, ErrMalformed
			}
		case 0x55:
			head := r.take(2)
			if r.err != nil {
				return nil, r.err
			}
			count := int(binary.LittleEndian.Uint16(head))
			ids := r.take(2 * count)
			if r.err != nil {
				return nil, r.err
			}
			op.NamedPropertyIDs = make([]uint16, count)
			for i := range op.NamedPropertyIDs {
				op.NamedPropertyIDs[i] = binary.LittleEndian.Uint16(ids[2*i:])
			}
		case 0x5f:
			flags := r.take(2)
			if r.err != nil {
				return nil, r.err
			}
			op.Flags = flags[0]
			if flags[1] != 0 {
				var set [16]byte
				copy(set[:], r.take(len(set)))
				if r.err != nil {
					return nil, r.err
				}
				op.NamedPropertySet = &set
			}
		case 0x56:
			head := r.take(3)
			if r.err != nil {
				return nil, r.err
			}
			op.Flags = head[0]
			count := int(binary.LittleEndian.Uint16(head[1:]))
			if count > maxNamedMappings || count > (len(r.data)-r.pos)/17 {
				return nil, ErrMalformed
			}
			for range count {
				name, err := parsePropertyName(&r)
				if err != nil {
					return nil, err
				}
				op.PropertyNames = append(op.PropertyNames, name)
			}
		default:
			label := publicROPLabels[op.ID]
			if label == "" {
				label = "other"
			}
			return nil, unsupportedOperation{label: label}
		}
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, op)
	}
	// Permit one archive commit per batch. A message save can be followed by
	// property reads and table navigation. An ICS delete permits Release only.
	// Decode and reserve the entire response before any effect.
	var committed byte
	for _, op := range out {
		if committed != 0 && op.ID != 1 && !(committed == 0x0c && canFollowMessageSave(op.ID)) {
			return nil, unsupportedOperation{
				label:                  SanitizeROPLabel(publicROPLabels[op.ID]),
				synchronizationOptions: SynchronizationOptionsDiagnostic{commitNonfinal: true},
			}
		}
		if op.ID == 0x0c || op.ID == 0x74 {
			committed = op.ID
		}
	}
	return out, r.done()
}

func canFollowMessageSave(id byte) bool {
	switch id {
	case 0x07, ropGetPropertiesAll, // Read properties of a just-saved message.
		0x04, 0x05, 0x21, 0x3e, // Open a hierarchy, contents, attachments or permissions table.
		0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b,
		0x38, 0x4f, 0x59, 0x5a, 0x6b, 0x6c, 0x81, 0x89: // Change or read the table view.
		return true
	default:
		return false
	}
}

func Failure(status uint32) []byte {
	if status == 0 {
		status = NotSupported
	}
	return append32(append32(nil, status), 0)
}
func ExecuteResponse(errorCode uint32, rop, aux []byte, maxOutput uint32) ([]byte, error) {
	if uint64(len(rop)) > uint64(maxOutput) || len(rop) > MaxBody || len(aux) > MaxAuxiliary {
		return nil, ErrMalformed
	}
	out := append32(append32(append32(nil, 0), errorCode), 0)
	return appendBlob(appendBlob(out, rop), aux), nil
}
func ConnectResponse(errorCode uint32, prefix, display string) ([]byte, error) {
	if len(prefix) > 4096 || len(display) > 4096 || bytes.IndexByte([]byte(prefix), 0) >= 0 {
		return nil, ErrMalformed
	}
	for _, b := range []byte(prefix) {
		if b > 127 {
			return nil, ErrMalformed
		}
	}
	out := []byte{}
	for _, n := range []uint32{0, errorCode, 60000, 0, 0} {
		out = append32(out, n)
	}
	out = append(append(out, []byte(prefix)...), 0)
	for _, c := range utf16.Encode([]rune(display)) {
		if c == 0 {
			return nil, ErrMalformed
		}
		out = binary.LittleEndian.AppendUint16(out, c)
	}
	out = binary.LittleEndian.AppendUint16(out, 0)
	return append32(out, 0), nil
}
