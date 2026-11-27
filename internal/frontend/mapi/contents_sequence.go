// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"maps"
	"slices"

	"ewsgo/internal/core"
)

// A Contents download retains the verified index and at most one encoded
// segment. Every GetBuffer uses its own authenticated request session.
type contentsSequence struct {
	prepared *contentsPreparation
	next     int
	cursor   int
}

func newContentsSequence(c synchronizationConfigure, prepared *contentsPreparation) (*contentsTransfer, error) {
	if err := validateContentsTransfer(c, prepared.delta); err != nil {
		return nil, err
	}
	prepared.delta.changes = orderedContentsChanges(c, prepared.delta.changes)
	next := 0
	if c.flags&0x8000 != 0 {
		next = -1 // the optional progress header precedes the first item
	}
	return &contentsTransfer{sequence: &contentsSequence{prepared: prepared, next: next}}, nil
}

func (d *contentsDownload) progressiveBuffer(ctx context.Context, session core.Session, store *ramStore, limit int) ([]byte, uint16, int, int, error) {
	transfer := d.transfer
	sequence := transfer.sequence
	delta := &sequence.prepared.delta
	total := len(delta.changes) + 1 // one step for every change and one for the trailer
	out := make([]byte, 0, limit)
	for len(out) < limit && sequence.next < total {
		if transfer.stream == nil {
			if limit-len(out) < 4 { // the first marker of any segment is an atom
				break
			}
			if err := d.prepareSegment(ctx, session, store, delta); err != nil {
				return nil, 0, 0, 0, err
			}
		}
		stream := transfer.stream
		end := stream.chunkEnd(sequence.cursor, limit-len(out))
		out = append(out, stream.data[sequence.cursor:end]...)
		sequence.cursor = end
		if end != len(stream.data) {
			break
		}
		sequence.next++
		sequence.cursor = 0
		transfer.stream = nil
		transfer.lease.release()
		transfer.lease = nil
	}
	done := max(0, sequence.next)
	if done == total {
		return out, 3, done, total, nil
	}
	if len(out) == 0 {
		return out, 2, done, total, nil
	}
	return out, 1, done, total, nil
}

func (d *contentsDownload) prepareSegment(ctx context.Context, session core.Session, store *ramStore, delta *contentsDelta) error {
	transfer := d.transfer
	i := transfer.sequence.next
	workspace := maxContentsFastTransferBytes
	hydrate := i >= 0 && i < len(delta.changes) && delta.changes[i].row.indexedMIME()
	if hydrate {
		workspace += core.MaxMailContentMIMEBytes + maxMailProjectionBytes
	}
	lease, err := store.reserveContentsPeak(ctx, workspace)
	if err != nil {
		return err
	}
	stream := &fastTransfer{limit: maxContentsFastTransferBytes}
	switch {
	case i < 0:
		err = writeContentsProgress(stream, delta.changes)
	case i == len(delta.changes):
		err = store.writeContentsTail(stream, d.config, *delta)
	default:
		change := delta.changes[i]
		if hydrate {
			change, err = store.loadIndexedContents(ctx, session, change.row)
		}
		if err == nil && change.row.special == 0 && nativeMeetingKind(change.row.object.Properties) != 0 {
			err = registerMeetingNames(ctx, &store.named)
		}
		if err == nil {
			err = store.writeContentsChange(stream, d.config, d.plan.selection, change)
		}
		if err == nil {
			transfer.normalChanges = transfer.normalChanges || !change.row.associated
			transfer.associatedChanges = transfer.associatedChanges || change.row.associated
		}
	}
	if err == nil {
		err = lease.shrink(len(stream.data) + 16*len(stream.atoms))
	}
	if err != nil {
		lease.release()
		return err
	}
	transfer.stream, transfer.lease = stream, lease
	return nil
}

func (s *ramStore) loadIndexedContents(ctx context.Context, session core.Session, row messageRow) (contentsChange, error) {
	reader, ok := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok {
		return contentsChange{}, core.ErrUnsupported
	}
	expected := row.object
	object, err := reader.ReadMailMessageMIME(ctx, expected.Parent, expected.ID, false)
	if err != nil {
		return contentsChange{}, err
	}
	if object.ID != expected.ID || object.Parent != expected.Parent || object.Revision != expected.Revision ||
		object.Kind != expected.Kind || object.Properties[core.IsRead] != expected.Properties[core.IsRead] {
		return contentsChange{}, core.ErrConflict
	}
	// The DAM flag belongs to the captured native queue, independently of
	// the exact MIME revision hydrated for this transfer item.
	if pending := expected.Properties[core.DeferredActionsPending]; pending != nil {
		if object.Properties == nil {
			object.Properties = make(core.Properties, 1)
		} else {
			object.Properties = maps.Clone(object.Properties)
		}
		object.Properties[core.DeferredActionsPending] = pending
	}
	planned, _, err := messageFingerprint(expected)
	if err != nil {
		return contentsChange{}, err
	}
	actual, _, err := messageFingerprint(object)
	if err != nil {
		return contentsChange{}, err
	}
	if planned != actual {
		return contentsChange{}, core.ErrConflict
	}
	row.object = object
	return s.projectContentsRow(row, session.Identity())
}

func hasIndexedChanges(changes []contentsChange) bool {
	return slices.ContainsFunc(changes, func(change contentsChange) bool { return change.row.indexedMIME() })
}
