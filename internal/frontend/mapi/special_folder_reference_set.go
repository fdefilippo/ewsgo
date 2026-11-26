// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"ewsgo/internal/core"
)

const (
	calendarReferenceTag  uint32 = 0x36d00102
	remindersReferenceTag uint32 = remindersOnlineEntryID<<16 | 0x0102
)

type specialFolderReferenceValue struct {
	entry []byte
	role  metadataFolderRole
	tag   uint32
}
type specialFolderReferenceSet struct{ values []specialFolderReferenceValue }

func (specialFolderReferenceSet) String() string   { return "specialFolderReferenceSet{redacted}" }
func (specialFolderReferenceSet) GoString() string { return "specialFolderReferenceSet{redacted}" }

// Decode the complete list of distinct enabled PtypBinary references before
// executing the batch. Only the Reminders search-folder reference can change.
func parseSpecialFolderReferenceSet(r *reader, enabled [metadataFolderRoleCount]bool, reminders bool) (*specialFolderReferenceSet, error) {
	h := r.take(4)
	if r.err != nil {
		return nil, ErrMalformed
	}
	size, count := int(binary.LittleEndian.Uint16(h)), int(binary.LittleEndian.Uint16(h[2:]))
	if size < 2 {
		return nil, ErrMalformed
	}
	data := r.take(size - 2)
	if r.err != nil {
		return nil, ErrMalformed
	}
	maximum := int(metadataFolderRoleCount)
	if reminders {
		maximum++
	}
	if count < 1 || count > maximum {
		return nil, unsupportedOperation{label: "RopSetProperties"}
	}
	out := &specialFolderReferenceSet{}
	v := reader{data: data}
	seen := make(map[uint32]bool, count)
	for i := 0; i < count; i++ {
		tag := v.u32()
		if v.err != nil {
			return nil, ErrMalformed
		}
		role, found := calendarMetadataRole, reminders && tag == remindersReferenceTag
		for candidate := metadataFolderRole(0); candidate < metadataFolderRoleCount; candidate++ {
			if enabled[candidate] && tag == candidate.tag() {
				role, found = candidate, true
				break
			}
		}
		if !found || seen[tag] {
			return nil, unsupportedOperation{label: "RopSetProperties"}
		}
		seen[tag] = true
		length := v.take(2)
		if v.err != nil {
			return nil, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint16(length))
		entry := v.take(n)
		if v.err != nil {
			return nil, ErrMalformed
		}
		value := specialFolderReferenceValue{tag: tag, role: role}
		if role.serviceRole() == MailboxRoot {
			if n > folderEntryIDBytes {
				return nil, unsupportedOperation{label: "RopSetProperties"}
			}
			value.entry = bytes.Clone(entry)
		}
		out.values = append(out.values, value)
	}
	if v.done() != nil {
		return nil, ErrMalformed
	}
	return out, nil
}

func setSpecialFolderReference(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, view *metadataBatchView) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	s := object.binding.store
	set := op.SpecialFolderReference
	if set == nil || len(set.values) == 0 || len(set.values) > int(metadataFolderRoleCount)+1 || object.isLogon || object.download != nil {
		return fail(NotSupported)
	}
	reminders := -1
	for index, value := range set.values {
		if value.tag == remindersReferenceTag {
			if view == nil || view.scope != s.scope {
				return fail(NotSupported)
			}
			reminders = index
		} else if !s.metadataRoleEnabled(value.role) {
			return fail(NotSupported)
		}
	}
	// Reject arbitrary source folders before a read can materialize any parent mapping.
	if object.fid != object.binding.fids[Inbox] && object.fid != object.binding.fids[MailboxRoot] {
		out := binary.LittleEndian.AppendUint16(fail(0), uint16(len(set.values)))
		for index, value := range set.values {
			out = binary.LittleEndian.AppendUint16(out, uint16(index))
			out = binary.LittleEndian.AppendUint32(out, value.tag)
			out = binary.LittleEndian.AppendUint32(out, accessDenied)
		}
		return out
	}
	if _, err := s.folder(ctx, session, object.fid); err != nil {
		return fail(storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	var statuses [metadataFolderRoleCount + 1]uint32
	var targets [metadataFolderRoleCount]core.ID
	loaded := false
	for index, value := range set.values {
		if index == reminders {
			continue
		}
		problem := uint32(0)

		if value.role.serviceRole() != MailboxRoot {
			problem = accessDenied
		} else if fid, err := parseFolderEntryID(value.entry, object.binding.mailboxGUID, s.replica); err != nil {
			problem = 0x80070057
		} else {
			s.mu.Lock()
			key, exists := s.byFID[fid]
			s.mu.Unlock()
			if !exists {
				problem = PropertyNotFound
			} else if key.native == "" {
				problem = accessDenied
			} else {
				if !loaded {
					native, err := s.metadataFolderObjects(ctx, session)
					if err != nil {
						return fail(storeError(err))
					}
					for _, o := range native {
						for role := metadataFolderRole(0); role < journalReferenceRole; role++ {
							if s.metadataRoleEnabled(role) && o.Properties[core.WellKnown] == core.Text(role.wellKnown()) {
								targets[role] = o.ID
							}
						}
					}
					loaded = true
				}
				if targets[value.role] == "" {
					if _, ok := session.Mailbox().(core.SpecialFolderReader); !ok && value.role != draftsReferenceRole {
						return fail(NotSupported)
					}
					return fail(PropertyNotFound)
				}
				if key.native != targets[value.role] || !bytes.Equal(value.entry, marshalFolderEntryID(object.binding.mailboxGUID, s.replica, fid)) {
					problem = accessDenied
				}
			}
		}
		statuses[index] = problem
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	if session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return fail(accessDenied)
	}
	if reminders >= 0 {
		code := writeFolderMetadata(ctx, session, object, folderMetadata{remindersOnlineEntryID: set.values[reminders].entry}, view)
		switch code {
		case 0, 0x80070057, PropertyNotFound:
			statuses[reminders] = code
		default:
			return fail(code)
		}
	}
	var problems []faiPropertyProblem
	for index, value := range set.values {
		if code := statuses[index]; code != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(index), tag: value.tag, code: code})
		}
	}
	return propertyProblemsResponse(op.ID, op.Input, problems)
}
