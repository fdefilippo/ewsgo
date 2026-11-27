// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"slices"
	"sort"

	"ewsgo/internal/core"
)

func indexSupportsContentsRestriction(n *contentsRestriction) bool {
	if n == nil {
		return true
	}
	if n.op <= 2 {
		return !slices.ContainsFunc(n.children, func(child *contentsRestriction) bool {
			return !indexSupportsContentsRestriction(child)
		})
	}
	if !n.bound {
		return false
	}
	switch n.property {
	case fixedContentsProperty(0x001a001f), fixedContentsProperty(0x0e060040), fixedContentsProperty(0x0e69000b),
		fixedContentsProperty(flagStatusTag), fixedContentsProperty(flagCompleteTimeTag), fixedContentsProperty(followupIconTag), fixedContentsProperty(hasDAMs),
		fixedContentsProperty(0x65e00102), fixedContentsProperty(0x30080040), fixedContentsProperty(0x65e20102),
		fixedContentsProperty(0x65e30102), fixedContentsProperty(0x67aa000b):
		return true
	case fixedContentsProperty(0x0e070003):
		// Only the attachment bit needs MIME. Exists and bitmask tests
		// that ignore it observe exactly the same result from the index.
		return n.op == 8 || n.op == 6 && n.mask&0x10 == 0
	}
	return false
}

func (s *ramStore) indexedContentsPayload(row messageRow, identity core.Identity) (projectedMessage, error) {
	size, ok := row.object.Properties[core.MessageSize].(core.Integer)
	if !ok || size < 0 {
		return projectedMessage{}, core.ErrInvalid
	}
	p, err := projectMailEnvelope(row.object)
	if err != nil {
		return projectedMessage{}, err
	}
	p.size = int(min(size, core.Integer(maxMailProjectionBytes)))
	hasAttachments := row.object.Properties[core.HasAttachments] == core.Bool(true)
	putContentsValue(p.values, 0x0e1b000b, core.Bool(hasAttachments))
	putContentsValue(p.values, 0x0e070003, core.Integer(mailContentsFlags(row.object, hasAttachments)))
	p.bytes += s.projectMeetingDelivery(p.values, row.object, identity)
	addContentsRowMetadata(&p, row)
	return p, nil
}

type contentsPreparation struct {
	delta              contentsDelta
	ticket             *contentsProofTicket
	normal, projection *contentsLease
	fai                *faiObservation
}

func (*contentsPreparation) String() string   { return "contentsPreparation{private}" }
func (*contentsPreparation) GoString() string { return "contentsPreparation{private}" }
func (p *contentsPreparation) close() {
	if p == nil {
		return
	}
	p.delta = contentsDelta{}
	_ = p.ticket.finish(context.Background(), false)
	p.ticket = nil
	p.normal.release()
	p.normal = nil
	p.projection.release()
	p.projection = nil
	p.fai.close()
	p.fai = nil
}

// This schema describes only data this profile can really project. In mixed
// Normal/FAI mode named restrictions are rejected: native MIME is not a general
// named-property source. An unknown property never means a known absent value.
func contentsProjectionSchema(normal, associated bool) func(contentsProperty) bool {
	return func(p contentsProperty) bool {
		if p.name != (propertyName{}) {
			return !normal && associated && p.id == 0 && (p.kind == 0x1f || p.kind == 0x102 || p.kind == 0xb || p.kind == 0x40)
		}
		tag := uint32(p.id)<<16 | uint32(p.kind)
		switch tag {
		case 0x65e00102, 0x30080040, 0x65e20102, 0x65e30102, 0x67aa000b:
			return true
		}
		if normal {
			switch tag {
			case flagStatusTag, flagCompleteTimeTag, followupIconTag, hasDAMs:
				return true
			case 0x003d001f, 0x0e1d001f, 0x0070001f, 0x00710102, 0x30130102, 0x3016000b:
				return true
			case 0x001a001f, 0x0037001f, 0x007d001f, 0x1035001f, 0x1000001f, 0x10130102, 0x3fde0003, 0x10160003, 0x00390040, 0x0e060040, 0x0e69000b, 0x0e1b000b, 0x0e070003, 0x0042001f, 0x0064001f, 0x0065001f, 0x5d02001f, 0x0c1a001f, 0x0c1e001f, 0x0c1f001f, 0x5d01001f:
				return true
			}
		}
		if associated {
			if p.kind != 0 && p.kind == associatedMetadataPropertyType(uint32(p.id)) {
				return true
			}
			switch tag {
			case 0x3001001f, 0x65ec001f: // Optional stored FAI metadata: DisplayName and RuleMessageName.
				return true
			case 0x003d001f, 0x0e1d001f:
				return true
			case 0x00170003, 0x00360003, 0x0e1f000b:
				return true
			case 0x0e070003, 0x0e060040:
				// Flags are derived from the local associated object. Delivery
				// time is known absent: FAI configurations are not delivered mail.
				return true
			case 0x001a001f, 0x0037001f, 0x7002001f, 0x7006001f, 0x0e69000b, 0x30070040, 0x70010102, 0x7c070102, 0x7c080102, 0x70070003, 0x7c060003:
				return true
			}
		}
		return false
	}
}

// Observations may advance the server's RAM version registry. They do not
// advance the client's uploaded state or publish a checkpoint on any failure.
func (s *contentsState) prepareContents(ctx context.Context, session core.Session, c synchronizationConfigure) (*contentsPreparation, error) {
	scope, err := contentsScopeFingerprint(s.root, c, s.plan)
	if err != nil || scope != s.scope {
		return nil, contentsPreparationError{contentsFailureState, core.ErrInvalid}
	}
	initial, err := s.beginPlanning(ctx, session)
	if err != nil {
		return nil, contentsPreparationError{contentsFailureState, err}
	}
	p := &contentsPreparation{}
	fail := func(stage contentsFailureStage, err error) (*contentsPreparation, error) {
		p.close()
		return nil, contentsPreparationError{stage, err}
	}
	var snapshots []messageSnapshot
	if s.plan.normal {
		// The source and registry detach one full view while the original is
		// still live. Reserve both before reading, then keep one result quota.
		p.normal, err = s.store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
		if err != nil {
			return fail(contentsFailureNormalMemory, err)
		}
		var view messageSnapshot
		switch s.plan.special {
		case core.ContactsFolder:
			view, err = s.store.observeContacts(ctx, session, s.root)
		case core.CalendarFolder:
			view, err = s.store.observeCalendarSearchItems(ctx, session, s.root, core.CalendarFolder)
		default:
			_, hasMIMEReader := session.Mailbox().(core.MailMessageMIMEReader)
			view, err = s.store.observeMailMessageSet(ctx, session, s.root, hasMIMEReader && indexSupportsContentsRestriction(s.plan.restriction))
		}
		if err != nil {
			return fail(contentsFailureNormalCapture, err)
		}
		if err = p.normal.shrink(view.retainedBytes); err != nil {
			return fail(contentsFailureNormalMemory, err)
		}
		snapshots = append(snapshots, view)
	}
	if s.plan.associated {
		p.fai, err = s.store.observeFAIMessages(ctx, session, s.root)
		if err != nil {
			return fail(contentsFailureFAICapture, err)
		}
		snapshots = append(snapshots, p.fai.snapshot)
	}
	// Existing projections plus the next bounded decode can coexist. A single
	// retained projection collection must fit 4 MiB before stream allocation.
	p.projection, err = s.store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return fail(contentsFailureProjectionMemory, err)
	}
	var changes []contentsChange
	known := map[uint64]messageKnown{}
	used := 1024 // Projection collection/map/slice overhead, including empty views.
	for _, snapshot := range snapshots {
		if snapshot.replica != s.store.replica || snapshot.root != s.root {
			return fail(contentsFailureDelta, core.ErrResync)
		}
		for _, set := range []idSet{initial.given, initial.seen, initial.seenFAI, initial.read} {
			for _, replica := range set.replicas {
				for _, r := range replica.ranges {
					if replica.replica != snapshot.replica || r.high > snapshot.highWater {
						return fail(contentsFailureDelta, core.ErrResync)
					}
				}
			}
		}
		for _, k := range snapshot.known {
			known[k.mid] = k
		}
		for _, row := range snapshot.messages {
			if err := ctx.Err(); err != nil {
				return fail(contentsFailureAuthorization, err)
			}
			change := contentsChange{row: row}
			if row.indexedMIME() {
				change.payload, err = s.store.indexedContentsPayload(row, session.Identity())
			} else {
				change, err = s.store.projectContentsRow(row, session.Identity())
			}
			if err != nil {
				return fail(contentsFailureProjection, err)
			}
			used += change.payload.bytes + 1024
			if used > maxMailProjectionBytes {
				return fail(contentsFailureProjectionMemory, core.ErrUnsupported)
			}
			changes = append(changes, change)
		}
	}
	if err = p.projection.shrink(used); err != nil {
		return fail(contentsFailureProjectionMemory, err)
	}
	if err = s.authorize(ctx, session); err != nil {
		return fail(contentsFailureAuthorization, err)
	}
	delta, err := planContentsDelta(s.store.replica, s.plan, c.flags, initial, changes, known)
	if err != nil {
		return fail(contentsFailureDelta, err)
	}
	for _, change := range delta.changes {
		props := change.row.object.Properties
		if change.row.special == core.CalendarFolder && props[core.AppointmentRecurring] == core.Bool(true) && props[appointmentRecur] == nil {
			// A calendar checkpoint must never acknowledge an incomplete series.
			return fail(contentsFailureProjection, core.ErrUnsupported)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(contentsFailureAuthorization, err)
	}
	p.ticket, err = s.store.stageContentsCheckpoint(s.scope, delta.final)
	if err != nil {
		return fail(contentsFailureCheckpoint, err)
	}
	if c.flags&2 == 0 {
		delta.retired = s.retiredGiven
	}
	if len(s.clientGiven) != 0 || c.flags&2 != 0 && len(s.retiredGiven) != 0 {
		// Echo provisional IDs on the wire without treating them as
		// downloaded messages in the server checkpoint.
		given := contentsRanges(delta.final.given)
		for _, count := range s.clientGiven {
			given = append(given, idRange{count, count})
		}
		if c.flags&2 != 0 {
			given = append(given, s.retiredGiven...)
		}
		delta.final.given = singleReplicaSet(s.store.replica, given)
	}
	p.delta = delta
	return p, nil
}

func planContentsDelta(replica [16]byte, plan contentsFilterPlan, flags uint16, initial contentsSyncState, changes []contentsChange, known map[uint64]messageKnown) (contentsDelta, error) {
	delta := contentsDelta{}
	for _, set := range []idSet{initial.given, initial.seen, initial.seenFAI, initial.read} {
		for _, r := range set.replicas {
			if r.replica != replica {
				return delta, core.ErrResync
			}
		}
	}
	// A previously delivered identity may only disappear through an explicit
	// deletion/no-longer block. Never silently forget an unaccounted identity.
	covered := uint64(0)
	for mid, k := range known {
		if mid != k.mid || mid == 0 || folderID(folderGlobalCount(mid)) != mid {
			return delta, core.ErrInvalid
		}
		if initial.given.contains(replica, folderGlobalCount(mid)) {
			if k.associated && !plan.associated || !k.associated && !plan.normal {
				return delta, core.ErrResync
			}
			covered++
		}
	}
	for _, r := range contentsRanges(initial.given) {
		if r.low == 0 || r.high < r.low || r.high-r.low+1 > covered {
			return delta, core.ErrResync
		}
		covered -= r.high - r.low + 1
	}
	if covered != 0 {
		return delta, core.ErrResync
	}
	current, matching := map[uint64]contentsChange{}, map[uint64]bool{}
	seen, seenFAI, read := contentsRanges(initial.seen), contentsRanges(initial.seenFAI), contentsRanges(initial.read)
	for _, change := range changes {
		row := change.row
		if _, duplicate := current[row.mid]; duplicate || row.associated && !plan.associated || !row.associated && !plan.normal {
			return contentsDelta{}, core.ErrInvalid
		}
		current[row.mid] = change
		match, err := plan.restriction.matches(change.payload.values)
		if err != nil {
			return contentsDelta{}, err
		}
		if !match {
			continue
		}
		matching[row.mid] = true
		id := folderGlobalCount(row.mid)
		seenSet := initial.seen
		if row.associated {
			seenSet = initial.seenFAI
		}
		fullChange := !initial.given.contains(replica, id) || !seenSet.contains(replica, row.cn)
		if fullChange {
			delta.changes = append(delta.changes, change)
			if row.associated {
				seenFAI = append(seenFAI, idRange{row.cn, row.cn})
			} else {
				seen = append(seen, idRange{row.cn, row.cn})
			}
		}
		if plan.readState && !row.associated && !initial.read.contains(replica, row.readCN) {
			// A full change already carries the message's read state. A
			// separate read delta applies only to an unchanged cached item.
			if !fullChange {
				if row.read {
					delta.read = append(delta.read, row.mid)
				} else {
					delta.unread = append(delta.unread, row.mid)
				}
			}
			read = append(read, idRange{row.readCN, row.readCN})
		}
	}
	var given []idRange
	for mid, k := range known {
		if k.associated && !plan.associated || !k.associated && !plan.normal {
			continue
		}
		id := folderGlobalCount(mid)
		if matching[mid] {
			given = append(given, idRange{id, id})
			continue
		}
		if !initial.given.contains(replica, id) {
			continue
		}
		_, inFolder := current[mid]
		if flags&2 != 0 || inFolder && flags&4 != 0 {
			// Suppressed deletions must not silently alter the client's Given.
			given = append(given, idRange{id, id})
			continue
		}
		if inFolder {
			delta.noLonger = append(delta.noLonger, mid)
		} else {
			delta.deleted = append(delta.deleted, mid)
		}
	}
	for mid := range matching {
		if _, ok := known[mid]; !ok {
			return contentsDelta{}, core.ErrInvalid
		}
	}
	delta.final = contentsSyncState{given: singleReplicaSet(replica, given), seen: singleReplicaSet(replica, seen), seenFAI: singleReplicaSet(replica, seenFAI), read: singleReplicaSet(replica, read)}
	for _, set := range []idSet{delta.final.given, delta.final.seen, delta.final.seenFAI, delta.final.read} {
		if _, err := set.marshal(); err != nil {
			return contentsDelta{}, err
		}
	}
	sort.Slice(delta.changes, func(i, j int) bool { return delta.changes[i].row.mid < delta.changes[j].row.mid })
	for _, ids := range [][]uint64{delta.deleted, delta.noLonger, delta.read, delta.unread} {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return delta, nil
}
func contentsRanges(set idSet) []idRange {
	var ranges []idRange
	for _, replica := range set.replicas {
		ranges = append(ranges, replica.ranges...)
	}
	return ranges
}
