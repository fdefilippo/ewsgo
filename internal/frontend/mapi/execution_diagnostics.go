// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"strconv"

	"ewsgo/internal/core"
)

// ExecutionDiagnostic retains only closed labels and bounded counters. It
// never retains response bytes, identities, handles, lengths or stream data.
type ExecutionDiagnostic struct {
	enabled               bool
	stage                 uint8
	ops                   [len(executionOperations)]bool
	firstErrorOp          uint8
	firstErrorKind        uint8
	buffers               uint32
	bufferCalls           uint16
	contentsCompletions   uint16
	contentsFailureStages uint16
	contentsBudgetFailure bool
	rejectedTable         rejectedTableFlags
	tableProfiles         uint32
	folderOpenProfiles    uint32
	notificationFlags     uint16
	notificationScopes    uint8
	notificationEmitted   uint16
	tableNotification     tableNotificationKind
	recoverableTable      recoverableTableDiagnostic
	attachmentTable       tableQueryDiagnostic
	searchFlags           uint32
	searchScopes          uint8
	searchRejections      uint8
	searchRestriction     contentsRestrictionRejection
	searchRestrictionType contentsRestrictionTypeRejection
	searchBinding         searchBindingRejection
	searchBindingType     string
	createMessageModes    uint8
	createMessageFolders  uint8
	submissionRequests    uint16
	deleteMessageOptions  uint8
	openStreamProperties  uint8
	openStreamObjects     uint8
	mailTableIdentity     uint8
	malformedROP          malformedOperation
	hierarchyImportTags   unknownPropertyTrace
}

// HierarchyImportProperties reports technical tags from refused hierarchy uploads.
func (d ExecutionDiagnostic) HierarchyImportProperties() UnknownPropertyDiagnostic {
	return d.hierarchyImportTags.fields()
}

// MalformedROP reports the rejected operation and its request-field class.
func (d ExecutionDiagnostic) MalformedROP() (operation, reason string) {
	return d.malformedROP.label, d.malformedROP.reason
}

func (d *ExecutionDiagnostic) recordMailTableIdentity(table *contentsTable) {
	if table == nil || table.hierarchy || table.associated || table.permissions != nil ||
		table.attachmentMessage != nil || table.flags&tableSoftDeletes != 0 {
		return
	}
	for _, tag := range table.columns {
		switch tag {
		case instanceServerIDTag:
			d.mailTableIdentity |= 1
		case folderEntryIDTag:
			d.mailTableIdentity |= 2
		case parentFolderEntryIDTag:
			d.mailTableIdentity |= 4
		case 0x0ff60102:
			d.mailTableIdentity |= 8
		}
	}
}

// MailTableIdentity lists the known identity columns requested on mail rows.
func (d ExecutionDiagnostic) MailTableIdentity() []string {
	var out []string
	for i, name := range [...]string{"instance_server_id", "entry_id", "parent_entry_id", "instance_key"} {
		if d.mailTableIdentity&(1<<i) != 0 {
			out = append(out, name)
		}
	}
	return out
}

func (d *ExecutionDiagnostic) recordOpenStream(objects *mailboxObjects, op Operation, handles []uint32) {
	property := 5
	switch op.Stream.tag {
	case messageBodyID<<16 | 0x001e:
		property = 0
	case messageBodyID<<16 | 0x001f:
		property = 1
	case messageHTMLID<<16 | 0x0102:
		property = 2
	case 0x10090102:
		property = 3
	case attachDataBinary:
		property = 4
	}
	d.openStreamProperties |= 1 << property
	object, code := objects.lookup(handles[op.Input], op.Logon)
	switch {
	case code != 0:
		d.openStreamObjects |= 1 << 0
	case object.attachment != nil:
		d.openStreamObjects |= 1 << 1
	case object.message != nil && object.message.contentVersioned:
		d.openStreamObjects |= 1 << 2
	case object.message != nil || object.download != nil:
		d.openStreamObjects |= 1 << 3
	case object.isLogon:
		d.openStreamObjects |= 1 << 4
	case object.table != nil || object.stream != nil || object.notification != nil:
		d.openStreamObjects |= 1 << 5
	default:
		d.openStreamObjects |= 1 << 6
	}
}

// OpenStreamProfile returns only closed classifications for an Execute batch.
func (d ExecutionDiagnostic) OpenStreamProfile() (properties, objects []string) {
	for i, label := range [...]string{"plain_string8", "plain_unicode", "html", "rtf", "attachment_data", "other"} {
		if d.openStreamProperties&(1<<i) != 0 {
			properties = append(properties, label)
		}
	}
	for i, label := range [...]string{"invalid_handle", "attachment", "native_mail", "other_message", "logon", "other_handle", "folder"} {
		if d.openStreamObjects&(1<<i) != 0 {
			objects = append(objects, label)
		}
	}
	return properties, objects
}

const (
	searchRequestLimit uint8 = 1 << iota
	searchRestrictionShape
	searchRestrictionBinding
	searchExpandedScopeLimit
)

func (d *ExecutionDiagnostic) recordSearchRejection(reason uint8) {
	if d.enabled {
		d.searchRejections |= reason
	}
}

func (d ExecutionDiagnostic) SearchRejections() []string {
	var reasons []string
	for i, reason := range [...]string{"request_limit", "restriction_shape", "restriction_binding", "expanded_scope_limit"} {
		if d.searchRejections&(1<<i) != 0 {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

func (d ExecutionDiagnostic) SearchRestrictionRejection() string {
	if d.searchRestriction == 0 {
		return ""
	}
	return d.searchRestriction.label()
}

// SearchRestrictionType returns only closed labels for a rejected property type.
func (d ExecutionDiagnostic) SearchRestrictionType() (operator, propertyType string) {
	return d.searchRestrictionType.operator, d.searchRestrictionType.propertyType
}

type searchBindingRejection byte

const (
	searchBindingType searchBindingRejection = iota + 1
	searchBindingReserved
	searchBindingUnregistered
	searchBindingNamed
	searchBindingFixed
)

func (d *ExecutionDiagnostic) recordSearchBinding(reason searchBindingRejection, kind uint16) {
	if d.enabled && d.searchBinding == 0 {
		d.searchBinding = reason
		d.searchBindingType = restrictionPropertyType(kind)
	}
}

// SearchBindingRejection returns closed labels for the first binding failure.
func (d ExecutionDiagnostic) SearchBindingRejection() (reason, propertyType string) {
	switch d.searchBinding {
	case searchBindingType:
		reason = "unsupported_type"
	case searchBindingReserved:
		reason = "reserved_property"
	case searchBindingUnregistered:
		reason = "unregistered_named_property"
	case searchBindingNamed:
		reason = "named_property_outside_schema"
	case searchBindingFixed:
		reason = "fixed_property_outside_schema"
	}
	return reason, d.searchBindingType
}

func (d *ExecutionDiagnostic) recordCreateMessage(objects *mailboxObjects, op Operation, handles []uint32) {
	if !d.enabled {
		return
	}
	d.createMessageModes |= 1 << min(op.Flags, 2)
	if op.Flags != 0 {
		return
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	store := object.binding.store
	if code != 0 || store == nil || object.download != nil {
		d.createMessageFolders |= 1 << 4
		return
	}
	if op.Folder != 0 && op.Folder == object.binding.fids[Outbox] {
		d.createMessageFolders |= 1
		return
	}
	store.mu.Lock()
	key, mapped := store.byFID[op.Folder]
	store.mu.Unlock()
	switch {
	case !mapped:
		d.createMessageFolders |= 1 << 3
	case key.native != "":
		d.createMessageFolders |= 1 << 1
	default:
		d.createMessageFolders |= 1 << 2
	}
}

func (d ExecutionDiagnostic) CreateMessageFolders() []string {
	var folders []string
	for i, folder := range [...]string{"outbox", "native", "other_local", "unmapped", "input_unavailable"} {
		if d.createMessageFolders&(1<<i) != 0 {
			folders = append(folders, folder)
		}
	}
	return folders
}

func (d *ExecutionDiagnostic) recordSubmission(objects *mailboxObjects, op Operation, handles []uint32) {
	if !d.enabled || op.ID == ropGetTransportFolder {
		return
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	switch {
	case code != 0:
		d.submissionRequests |= 1 << 8
	case object.isLogon:
		d.submissionRequests |= 1 << 7
	case object.message != nil:
		d.submissionRequests |= 1 << 6
	default:
		d.submissionRequests |= 1 << 9
	}
	if op.ID == ropTransportSend {
		d.submissionRequests |= 1 << 5
	} else {
		d.submissionRequests |= 1 << min(op.Flags, 4)
	}
}

func (d ExecutionDiagnostic) SubmissionRequests() []string {
	var requests []string
	for i, label := range [...]string{
		"flags:none", "flags:preprocess", "flags:spooler", "flags:preprocess_spooler",
		"flags:other", "transport_send", "object:message", "object:logon",
		"object:unavailable", "object:other",
	} {
		if d.submissionRequests&(1<<i) != 0 {
			requests = append(requests, label)
		}
	}
	return requests
}

func (d ExecutionDiagnostic) CreateMessageModes() []string {
	var modes []string
	for i, mode := range [...]string{"normal", "associated", "invalid"} {
		if d.createMessageModes&(1<<i) != 0 {
			modes = append(modes, mode)
		}
	}
	return modes
}

func (d *ExecutionDiagnostic) recordDeleteMessageOptions(request deleteMessagesRequest) {
	if !d.enabled {
		return
	}
	mode := 0
	if request.async {
		mode |= 1
	}
	if request.notifyNonRead {
		mode |= 2
	}
	d.deleteMessageOptions |= 1 << mode
}

// DeleteMessageOptions lists the option combinations for delete requests in the batch.
func (d ExecutionDiagnostic) DeleteMessageOptions() []string {
	var modes []string
	for i, mode := range [...]string{"synchronous", "asynchronous", "synchronous_notify_non_read", "asynchronous_notify_non_read"} {
		if d.deleteMessageOptions&(1<<i) != 0 {
			modes = append(modes, mode)
		}
	}
	return modes
}

func (d *ExecutionDiagnostic) recordSearchRequest(r searchCriteriaOperation) {
	if !d.enabled {
		return
	}
	d.searchFlags |= r.flags
	switch {
	case r.overLimit:
		d.searchScopes |= 8
	case len(r.folders) == 0:
		d.searchScopes |= 1
	case len(r.folders) == 1:
		d.searchScopes |= 2
	default:
		d.searchScopes |= 4
	}
}

func (d ExecutionDiagnostic) SearchOptions() (flags, scopes []string) {
	if d.searchScopes == 0 {
		return nil, nil
	}
	for _, flag := range [...]struct {
		bit   uint32
		label string
	}{
		{searchStop, "stop"}, {searchRestart, "restart"}, {searchRecursive, "recursive"},
		{searchShallow, "shallow"}, {searchForeground, "foreground"}, {searchBackground, "background"},
		{searchIndexed, "indexed"}, {searchSequential, "sequential"}, {searchStatic, "static"},
	} {
		if d.searchFlags&flag.bit != 0 {
			flags = append(flags, flag.label)
			d.searchFlags &^= flag.bit
		}
	}
	if d.searchFlags != 0 {
		flags = append(flags, "unknown")
	}
	for bit, size := range [...]string{"none", "one", "several", "over_limit"} {
		if d.searchScopes&(1<<bit) != 0 {
			scopes = append(scopes, size)
		}
	}
	return flags, scopes
}

// Only the sequential decoder can populate this field, after a complete open
// header and both handle indices have been checked. It is not a byte scanner.
type rejectedTableFlags struct{ opcode, flags byte }

func (rejectedTableFlags) String() string   { return "rejectedTableFlags{redacted}" }
func (rejectedTableFlags) GoString() string { return "rejectedTableFlags{redacted}" }

// RejectedTableFlags describes the failing operation, independently of the
// first-ROP inspector. The opcode itself is the existing rop_unsupported label.
func (d ExecutionDiagnostic) RejectedTableFlags() []string {
	if !d.enabled || d.rejectedTable.opcode == 0 {
		return []string{"none"}
	}
	return inspectTableFlags([]byte{d.rejectedTable.opcode, 0, 0, 0, d.rejectedTable.flags}, 1)
}

func (ExecutionDiagnostic) String() string   { return "ExecutionDiagnostic{redacted}" }
func (ExecutionDiagnostic) GoString() string { return "ExecutionDiagnostic{redacted}" }

var executionOperations = [...]struct {
	id    byte
	label string
}{
	{1, "release"}, {2, "open_folder"}, {0x07, "get_properties"},
	{ropSetProperties, "set_properties"}, {ropSetPropertiesNoReplicate, "set_properties_no_replicate"}, {0x0c, "save_message"}, {0x1c, "create_folder"},
	{ropDeleteProperties, "delete_properties"}, {ropDeletePropertiesNoReplicate, "delete_properties_no_replicate"},
	{0x0d, "remove_all_recipients"}, {0x0e, "modify_recipients"}, {0x0f, "read_recipients"},
	{0x27, "get_receive_folder"}, {0x43, "long_term_id_from_id"}, {0x44, "id_from_long_term_id"},
	{ropSetReceiveFolder, "set_receive_folder"},
	{0x49, "get_address_types"}, {0x4d, "fast_transfer_copy_to"}, {0x4e, "get_buffer"}, {0x56, "get_property_ids"},
	{ropFastTransferSourceCopyProperties, "fast_transfer_copy_properties"},
	{ropFastTransferSourceCopyMessages, "fast_transfer_copy_messages"},
	{ropFastTransferSourceCopyFolder, "fast_transfer_copy_folder"},
	{ropTellVersion, "tell_version"},
	{ropOptionsData, "options_data"},
	{ropDeleteMessages, "delete_messages"},
	{ropSetMessageReadFlag, "set_message_read_flag"}, {ropSetReadFlags, "set_read_flags"},
	{ropMoveCopyMessages, "move_copy_messages"}, {0x70, "configure_sync"}, {0x72, "import_message"}, {0x74, "import_delete"},
	{0x75, "upload_state_begin"}, {0x76, "upload_state_continue"}, {0x77, "upload_state_end"},
	{0x7e, "open_collector"}, {0x7f, "get_local_replica_ids"}, {0x82, "get_transfer_state"},
	{0xfe, "logon"},
	{0x05, "get_contents_table"}, {0x12, "set_columns"}, {0x15, "query_rows"},
	{0x16, "get_table_status"}, {0x17, "query_position"}, {0x18, "seek_row"},
	{0x80, "import_read_state"}, {0x04, "get_hierarchy_table"}, {0x3e, "get_permissions_table"},
	{0x3f, "get_rules_table"}, {0x40, "modify_permissions"}, {0x21, "get_attachment_table"},
	{0x52, "get_valid_attachments"},
	{0x2b, "open_stream"}, {0x2c, "read_stream"}, {0x2e, "seek_stream"}, {0x5e, "get_stream_size"},
	{0x22, "open_attachment"}, {ropCreateAttachment, "create_attachment"}, {ropSaveAttachment, "save_attachment"},
	{ropDeleteAttachment, "delete_attachment"},
	{ropWriteStream, "write_stream"}, {ropSetStreamSize, "set_stream_size"}, {ropCommitStream, "commit_stream"},
	{ropWriteAndCommitStream, "write_and_commit_stream"},
	{ropCopyToStream, "copy_to_stream"},
	{ropCloneStream, "clone_stream"},
	{ropLockRegionStream, "lock_region_stream"}, {ropUnlockRegionStream, "unlock_region_stream"},
	{ropProgress, "progress"},
	{ropSubmitMessage, "submit_message"}, {ropTransportSend, "transport_send"}, {ropGetTransportFolder, "get_transport_folder"},
	{ropAbortSubmit, "abort_submit"}, {ropSetSpooler, "set_spooler"},
	{ropSpoolerLockMessage, "spooler_lock_message"}, {ropTransportNewMail, "transport_new_mail"},
	{ropOpenEmbeddedMessage, "open_embedded_message"}, {ropUpdateDeferredActions, "update_deferred_action_messages"},
	{ropModifyRules, "modify_rules"},
	{ropSetLocalReplicaMidsetDeleted, "set_local_replica_midset_deleted"},
	{0x13, "sort_table"},
	{0x14, "restrict_table"}, {0x4f, "find_row"},
	{0x73, "import_hierarchy"}, {0x78, "import_message_move"},
	{ropFastTransferDestinationConfigure, "configure_upload"}, {ropFastTransferDestinationPutBuffer, "put_buffer"},
	{0x03, "open_message"},
	{ropReloadCachedInformation, "reload_cached_information"},
	{ropGetMessageStatus, "get_message_status"},
	{ropSetMessageStatus, "set_message_status"},
	{ropCopyTo, "copy_to"}, {ropCopyProperties, "copy_properties"},
	{0x29, "register_notification"},
	{ropGetPropertiesAll, "get_properties_all"}, {0x09, "get_properties_list"}, {0x37, "query_columns_all"},
	{0x68, "get_receive_folder_table"}, {ropGetStoreState, "get_store_state"},
	{ropGetPerUserLongTermIDs, "get_per_user_long_term_ids"}, {ropGetPerUserGUID, "get_per_user_guid"},
	{ropReadPerUserInformation, "read_per_user_information"}, {ropWritePerUserInformation, "write_per_user_information"},
	{ropGetOwningServers, "get_owning_servers"}, {ropPublicFolderIsGhosted, "public_folder_is_ghosted"},
	{0x19, "seek_row_bookmark"}, {0x1a, "seek_row_fractional"}, {0x1b, "create_bookmark"},
	{0x38, "abort_table"}, {0x81, "reset_table"}, {0x89, "free_bookmark"},
	{0x59, "expand_row"}, {0x5a, "collapse_row"},
	{0x6b, "get_collapse_state"}, {0x6c, "set_collapse_state"},
	{0x55, "get_names_from_property_ids"}, {0x5f, "query_named_properties"},
	{ropDeleteFolder, "delete_folder"},
	{ropMoveFolder, "move_folder"},
	{ropCopyFolder, "copy_folder"},
	{ropEmptyFolder, "empty_folder"},
	{ropHardDeleteMessages, "hard_delete_messages"},
	{ropHardDeleteFolderContents, "hard_delete_messages_and_subfolders"},
	{ropSetSearchCriteria, "set_search_criteria"},
	{ropGetSearchCriteria, "get_search_criteria"},
	{0x06, "create_message"},
}

func (d *ExecutionDiagnostic) recordNotificationRequest(r notificationRequest) {
	if !d.enabled {
		return
	}
	d.notificationFlags |= r.mask
	switch {
	case r.whole:
		d.notificationScopes |= 1
	case r.mid != 0:
		d.notificationScopes |= 4
	default:
		d.notificationScopes |= 2
	}
}

// Registration summaries retain protocol flags and scope categories only.
func (d ExecutionDiagnostic) NotificationRegistration() (events, scopes []string) {
	if d.notificationScopes == 0 {
		return nil, nil
	}
	for bit, event := range [...]string{"", "new_mail", "created", "deleted", "modified", "moved", "copied", "search_completed", "table_modified", "", "extended"} {
		if event != "" && d.notificationFlags&(1<<bit) != 0 {
			events = append(events, event)
		}
	}
	if d.notificationFlags & ^uint16(knownEventMask) != 0 {
		events = append(events, "unknown")
	}
	for bit, scope := range [...]string{"mailbox", "folder", "message"} {
		if d.notificationScopes&(1<<bit) != 0 {
			scopes = append(scopes, scope)
		}
	}
	return events, scopes
}

func (d ExecutionDiagnostic) NotificationDelivery() []string {
	if !d.enabled {
		return nil
	}
	var events []string
	for kind, name := range [...]string{"hierarchy_table_changed", "contents_table_changed", "hierarchy_table_row_added", "hierarchy_table_row_modified", "table_notification_pending"} {
		if d.tableNotification&(1<<kind) != 0 {
			events = append(events, name)
		}
	}
	for index, kind := range [...]string{"folder", "message"} {
		mask := uint16(d.notificationEmitted >> (8 * index))
		for i, event := range [...]string{"created", "deleted", "modified", "moved"} {
			if mask&(notifyCreated<<i) != 0 {
				events = append(events, kind+"_"+event)
			}
		}
	}
	if d.notificationEmitted&notifySearchCompleted != 0 {
		events = append(events, "search_completed")
	}
	if d.notificationEmitted&(notifyNewMail<<8) != 0 {
		events = append(events, "new_mail")
	}
	return events
}

func (d *ExecutionDiagnostic) at(stage uint8) {
	if d.enabled {
		d.stage = stage
	}
}

func (d *ExecutionDiagnostic) record(id byte, response []byte) {
	if !d.enabled {
		return
	}
	for i, op := range executionOperations {
		if id != op.id {
			continue
		}
		d.ops[i] = true
		if id == 1 || d.firstErrorOp != 0 || len(response) < 6 {
			return
		}
		code := binary.LittleEndian.Uint32(response[2:])
		if code == 0 {
			return
		}
		d.firstErrorOp = uint8(i + 1)
		d.firstErrorKind = 5
		switch code {
		case NotSupported:
			d.firstErrorKind = 1
		case accessDenied:
			d.firstErrorKind = 2
		case 0x8004010f:
			d.firstErrorKind = 3
		case 0x80070057:
			d.firstErrorKind = 4
		case invalidObject:
			d.firstErrorKind = 6
		case nullObject:
			d.firstErrorKind = 7
		case ropNotImplemented:
			d.firstErrorKind = 8
		case searchNotInitialized:
			d.firstErrorKind = 9
		case searchScopeViolation:
			d.firstErrorKind = 10
		case searchRestrictionTooComplex:
			d.firstErrorKind = 11
		case notSearchFolder:
			d.firstErrorKind = 12
		case 0x8007000e:
			d.firstErrorKind = 13
		}
		return
	}
}

// Classify the existing handle in memory; no native read or new authorization.
func executionBufferKind(objects *mailboxObjects, op Operation, handles []uint32) uint8 {
	o, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 || o.download == nil {
		return 0
	}
	d := o.download
	if d.collector != nil || d.draft != nil || d.hierarchyCollector {
		return 0
	}
	if d.contents != nil {
		if d.contents.state == nil && d.contents.transfer != nil {
			return 3 // Immutable Contents/FAI GetTransferState source.
		}
		return 2
	}
	if d.state != nil {
		return 1
	}
	if d.copySource != nil || d.copyFolder != "" {
		return 4
	}
	if d.stream != nil {
		return 3
	}
	return 0
}

func (d *ExecutionDiagnostic) recordBuffer(kind uint8, response []byte) {
	if !d.enabled || d.bufferCalls >= 1024 {
		return
	}
	d.bufferCalls++
	if kind > 4 {
		kind = 0
	}
	outcome := uint8(0) // Error includes malformed internal response, never Done.
	if len(response) >= 15 && response[0] == 0x4e && binary.LittleEndian.Uint32(response[2:]) == 0 && response[12] == 0 && int(binary.LittleEndian.Uint16(response[13:])) == len(response)-15 {
		status := binary.LittleEndian.Uint16(response[6:])
		if status >= 1 && status <= 3 {
			outcome = uint8(status)
		} else {
			outcome = 4
		}
	}
	d.buffers |= 1 << (kind*5 + outcome)
}

// Called after GetBuffer, before a later ROP can release or replace its handle.
// Presence means the completed server stream contained changes; it does not
// assert that Outlook received, parsed or applied them. No source I/O occurs.
func (d *ExecutionDiagnostic) recordContentsCompletion(objects *mailboxObjects, op Operation, handles []uint32, response []byte) {
	if !d.enabled || len(response) < 15 || response[0] != 0x4e || binary.LittleEndian.Uint32(response[2:]) != 0 || binary.LittleEndian.Uint16(response[6:]) != 3 || response[12] != 0 || int(binary.LittleEndian.Uint16(response[13:])) != len(response)-15 {
		return
	}
	o, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 || o.download == nil || o.download.contents == nil {
		return
	}
	c := o.download.contents
	if c.state == nil || c.transfer == nil || !c.completed {
		return // Immutable state-only exports and failed transfers have no summary.
	}
	if c.plan.normal {
		bit := uint8(2) // Other Normal source, empty.
		switch {
		case c.plan.special == core.ContactsFolder:
			bit = 6
		case c.plan.special == core.CalendarFolder:
			bit = 8
		case o.fid == o.binding.fids[Inbox]:
			bit = 0
		}
		if c.transfer.normalChanges {
			bit++
		}
		d.contentsCompletions |= 1 << bit
	}
	if c.plan.associated {
		bit := uint8(4)
		if c.transfer.associatedChanges {
			bit++
		}
		d.contentsCompletions |= 1 << bit
	}
}

func (d ExecutionDiagnostic) Fields() (stage string, ops []string, firstError string, buffers []string, calls string) {
	stages := [...]string{"none", "request_rejected", "framing_rejected", "batch_rejected", "output_limit", "output_too_small", "executed"}
	stage = "other"
	if int(d.stage) < len(stages) {
		stage = stages[d.stage]
	}
	ops = []string{}
	for i, op := range executionOperations {
		if d.ops[i] {
			ops = append(ops, op.label)
		}
	}
	if len(ops) == 0 {
		ops = []string{"none"}
	}
	firstError = "none"
	if d.firstErrorOp != 0 || d.firstErrorKind != 0 {
		firstError = "other"
		kinds := [...]string{"none", "not_supported", "access_denied", "not_found", "invalid_parameter", "other", "invalid_object", "null_object", "not_implemented",
			"search_not_initialized", "search_scope_violation", "search_restriction_too_complex", "not_search_folder", "out_of_memory"}
		if d.firstErrorOp > 0 && int(d.firstErrorOp) <= len(executionOperations) && d.firstErrorKind > 0 && int(d.firstErrorKind) < len(kinds) {
			firstError = executionOperations[d.firstErrorOp-1].label + ":" + kinds[d.firstErrorKind]
		}
	}
	buffers = []string{}
	for k, kind := range [...]string{"unknown", "hierarchy", "contents", "state", "copy_to"} {
		for s, status := range [...]string{"error", "partial", "no_room", "done", "other"} {
			if d.buffers&(1<<(k*5+s)) != 0 {
				buffers = append(buffers, kind+":"+status)
			}
		}
	}
	if d.buffers>>25 != 0 {
		buffers = append(buffers, "other")
	}
	completions := [...]string{
		"inbox_normal_empty:done", "inbox_normal_nonempty:done", "other_normal_empty:done", "other_normal_nonempty:done",
		"associated_empty:done", "associated_nonempty:done", "contacts_normal_empty:done", "contacts_normal_nonempty:done",
		"calendar_normal_empty:done", "calendar_normal_nonempty:done",
	}
	for bit, label := range completions {
		if d.contentsCompletions&(1<<bit) != 0 {
			buffers = append(buffers, label)
		}
	}
	if d.contentsCompletions>>len(completions) != 0 {
		buffers = append(buffers, "other")
	}
	for stage, label := range contentsFailureLabels {
		if stage > 0 && d.contentsFailureStages&(1<<stage) != 0 {
			buffers = append(buffers, "contents_failure:"+label)
		}
	}
	if d.contentsBudgetFailure {
		buffers = append(buffers, "contents_failure:memory_budget")
	}
	if len(buffers) == 0 {
		buffers = []string{"none"}
	}
	calls = "other"
	if d.bufferCalls <= 1024 {
		calls = strconv.Itoa(int(d.bufferCalls))
	}
	return
}
