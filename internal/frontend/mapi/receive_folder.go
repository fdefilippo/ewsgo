// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var receiveAssociations = [...]struct {
	class string
	role  SpecialFolderRole
}{
	{"Report.IPM", Inbox}, {"IPM", Inbox}, {"IPC", MailboxRoot}, {"", Inbox},
}

const ropSetReceiveFolder = 0x26

const receiveTableResponseSize = 10 + len(receiveAssociations)*(1+8+1+8) + len("Report.IPM") + len("IPM") + len("IPC")

// MS-OXCSTOR 2.2.1.2.1: at most 255 ASCII bytes including the terminator.
func validReceiveClass(value []byte) bool {
	if len(value) > 254 {
		return false
	}
	for _, b := range value {
		if b < 32 || b > 126 {
			return false
		}
	}
	return len(value) == 0 || value[0] != '.' && value[len(value)-1] != '.' && !bytes.Contains(value, []byte(".."))
}

// A read-only projection of the initial associations in MS-OXCSTOR 3.2.3.
// There is no mutable routing table or claim of delivery/filter execution.
func receiveAssociation(messageClass string) (SpecialFolderRole, string) {
	upper := strings.ToUpper(messageClass)
	for _, association := range receiveAssociations {
		if strings.HasPrefix(upper, strings.ToUpper(association.class)) {
			return association.role, association.class
		}
	}
	return Inbox, ""
}

func getReceiveFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0x27, op.Input, code) }
	object, code := objects.logonObject(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if op.InvalidMessageClass {
		return fail(0x80070057)
	}
	if err := authorizeReceiveFolders(ctx, session, object.binding); err != nil {
		return fail(storeError(err))
	}
	rows, err := effectiveReceiveFolders(ctx, session, object.binding, false)
	if err != nil {
		return fail(storeError(err))
	}
	class := strings.ToUpper(op.MessageClass)
	best := receiveFolderRow{}
	for _, row := range rows {
		prefix := strings.ToUpper(row.class)
		if (best.fid == 0 || len(prefix) > len(best.class)) && strings.HasPrefix(class, prefix) {
			best = row
		}
	}
	if best.fid == 0 {
		return fail(0x00000463)
	}
	out := binary.LittleEndian.AppendUint64(fail(0), best.fid)
	return append(append(out, best.class...), 0)
}

func setReceiveFolder(ctx context.Context, session core.Session, objects *mailboxObjects, operation Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(operation.ID, operation.Input, code) }
	object, code := objects.logonObject(handles[operation.Input], operation.Logon)
	if code != 0 {
		return fail(code)
	}
	if operation.InvalidMessageClass {
		return fail(0x80070057)
	}
	router, ok := session.Mailbox().(core.ReceiveFolderMailbox)
	if !ok {
		return fail(NotSupported)
	}
	if err := authorizeReceiveFolders(ctx, session, object.binding); err != nil {
		return fail(storeError(err))
	}
	class := operation.MessageClass
	if strings.EqualFold(class, "IPM") || strings.EqualFold(class, "Report.IPM") {
		return fail(accessDenied)
	}
	if operation.Folder == 0 && class == "" {
		return fail(0x80004005)
	}
	var destination core.ID
	if operation.Folder != 0 {
		folder, err := object.binding.store.folder(ctx, session, operation.Folder)
		if err != nil {
			return fail(storeError(err))
		}
		if folder.native == "" {
			return fail(NotSupported)
		}
		destination = folder.native
	}
	return fail(storeErrorOrSuccess(router.SetReceiveFolder(ctx, class, destination)))
}

func authorizeReceiveFolders(ctx context.Context, session core.Session, binding storeBinding) error {
	if _, err := binding.store.folder(ctx, session, binding.fids[MailboxRoot]); err != nil {
		return err
	}
	return hierarchyOwner(ctx, session.Mailbox())
}

// Resolve the current native Inbox after removal, role reassignment or access changes.
func resolveReceiveInbox(ctx context.Context, session core.Session, store *ramStore) (uint64, error) {
	catalog, err := privateFolders(ctx, session)
	if err != nil {
		return 0, err
	}
	if catalog.scope != store.scope || catalog.folders[IPMSubtree].NativeID != store.root {
		return 0, core.ErrAccessDenied
	}
	native := catalog.folders[Inbox].NativeID
	current, err := session.Mailbox().Get(ctx, native)
	if err != nil {
		return 0, err
	}
	if current.ID != native || current.Kind != core.Folder || current.Parent == "" || current.Parent == native {
		return 0, core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ids, err := store.assign(ctx, []folderKey{{native: native}})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func getReceiveFolderTable(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.logonObject(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if err := authorizeReceiveFolders(ctx, session, object.binding); err != nil {
		return fail(storeError(err))
	}
	rows, err := effectiveReceiveFolders(ctx, session, object.binding, true)
	if err != nil {
		return fail(storeError(err))
	}
	out := append32(fail(0), uint32(len(rows)))
	for _, row := range rows {
		out = binary.LittleEndian.AppendUint64(append(out, 0), row.fid) // Standard PropertyRow.
		out = append(append(out, row.class...), 0)
		out = binary.LittleEndian.AppendUint64(out, row.modified)
	}
	if op.ReceiveTableLimit > 0 && len(out) > op.ReceiveTableLimit {
		return fail(tableBufferTooSmall)
	}
	return out
}

type receiveFolderRow struct {
	class    string
	fid      uint64
	modified uint64
}

func effectiveReceiveFolders(ctx context.Context, session core.Session, binding storeBinding, dates bool) ([]receiveFolderRow, error) {
	inbox, err := resolveReceiveInbox(ctx, session, binding.store)
	if err != nil {
		return nil, err
	}
	var rootSince, inboxSince uint64
	if dates {
		rootSince, inboxSince = binding.store.receiveFolderDates(inbox)
	}
	rows := make([]receiveFolderRow, 0, len(receiveAssociations))
	for _, association := range receiveAssociations {
		fid, modified := inbox, inboxSince
		if association.role == MailboxRoot {
			fid, modified = binding.fids[MailboxRoot], rootSince
		}
		rows = append(rows, receiveFolderRow{association.class, fid, modified})
	}
	router, ok := session.Mailbox().(core.ReceiveFolderMailbox)
	if !ok {
		return rows, ctx.Err()
	}
	overrides, err := router.ReadReceiveFolders(ctx)
	if errors.Is(err, core.ErrUnsupported) {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}
	keys := make([]folderKey, len(overrides))
	for i, association := range overrides {
		if !validReceiveClass([]byte(association.Class)) || association.Folder == "" ||
			strings.EqualFold(association.Class, "IPM") || strings.EqualFold(association.Class, "Report.IPM") {
			return nil, core.ErrInvalid
		}
		keys[i].native = association.Folder
	}
	fids, err := binding.store.assign(ctx, keys)
	if err != nil {
		return nil, err
	}
	for i, association := range overrides {
		row := receiveFolderRow{association.Class, fids[i], fileTime(association.Modified)}
		replaced := false
		for j := range rows {
			if strings.EqualFold(rows[j].class, association.Class) {
				rows[j], replaced = row, true
				break
			}
		}
		if !replaced {
			rows = append(rows, row)
		}
	}
	return rows, ctx.Err()
}

// The table describes EWSGo's routing projection. Zimbra has no timestamp for
// these mappings. Keep the first observation and advance only on remapping.
func (s *ramStore) receiveFolderDates(inbox uint64) (root, inboxModified uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receiveRootSince == 0 {
		s.receiveRootSince = fileTime(time.Now())
	}
	if s.receiveInbox != inbox {
		s.receiveInbox, s.receiveInboxSince = inbox, max(s.receiveInboxSince+1, fileTime(time.Now()))
	}
	return s.receiveRootSince, s.receiveInboxSince
}
