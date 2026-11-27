// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

var meetingDraftProperty = core.Key{Set: core.MeetingMessageType.Set, Name: "mapiMeetingTransport"}

type meetingDraftStore interface {
	DraftProperties(context.Context, string, string) (map[core.Key]core.Text, error)
	RegisterDraftProperties(context.Context, string, map[core.Key]core.Text) (string, error)
	SetDraftStringProperty(context.Context, string, string, core.Key, core.Text) error
}

type meetingMailDraft struct {
	Class             string
	UID               string
	Start, End        time.Time
	Location          string
	Sequence          int32
	AllDay, Recurring bool
	ResponseRequested *bool
	// Conflicting GOIDs or an occurrence GOID require an unsupported path.
	InvalidIdentity bool
}

func draftMeetingKind(class string) core.MeetingMessageKind {
	switch strings.ToLower(class) {
	case "ipm.schedule.meeting.request":
		return core.MeetingRequestMessage
	case "ipm.schedule.meeting.canceled":
		return core.MeetingCancellationMessage
	case "ipm.schedule.meeting.resp.pos":
		return core.MeetingAcceptedMessage
	case "ipm.schedule.meeting.resp.tent":
		return core.MeetingTentativeMessage
	case "ipm.schedule.meeting.resp.neg":
		return core.MeetingDeclinedMessage
	default:
		return 0
	}
}

// SetProperties mixes content with metadata. Keep the semantic fields even
// if MessageClass is set in a later ROP on this handle.
func (m *meetingMailDraft) set(names *namedPropertyRegistry, value faiWireProperty) (uint32, bool) {
	property, err := resolveContentsIdentity(value.tag, names)
	if err != nil || !contentsValueType(property.kind, value.value) {
		return 0, false
	}
	switch property {
	case fixedContentsProperty(0x001a001f):
		class := string(value.value.(core.Text))
		if strings.EqualFold(class, "IPM.Note") {
			if m.Class != "" {
				return NotSupported, true
			}
			return 0, true
		}
		kind := draftMeetingKind(class)
		if kind == 0 && !strings.HasPrefix(strings.ToLower(class), "ipm.schedule.meeting.") {
			return NotSupported, true
		}
		if len(class) > 128 || !validMailText(class) {
			return 0x80070057, true
		}
		m.Class = class
	case calendarSearchNamed(appointmentGlobalID, 0x0102).property, calendarSearchNamed(appointmentCleanID, 0x0102).property:
		id, err := appointmentUID(value.value.(core.Binary))
		if err != nil || m.UID != "" && m.UID != id {
			m.InvalidIdentity = true
			return NotSupported, true
		}
		m.UID = id
	case fixedContentsProperty(appointmentStartDateTag), calendarSearchNamed(appointmentStartWhole, 0x0040).property,
		calendarSearchNamed(appointmentStart, 0x0040).property:
		m.Start = time.Time(value.value.(core.Timestamp))
	case fixedContentsProperty(appointmentEndDateTag), calendarSearchNamed(appointmentEndWhole, 0x0040).property,
		calendarSearchNamed(appointmentEnd, 0x0040).property:
		m.End = time.Time(value.value.(core.Timestamp))
	case calendarSearchNamed(appointmentLocation, 0x001f).property:
		location := string(value.value.(core.Text))
		if len(location) > maxHierarchyText {
			return 0x80070057, true
		}
		m.Location = location
	case calendarSearchNamed(appointmentAllDay, 0x000b).property:
		m.AllDay = bool(value.value.(core.Bool))
	case calendarSearchNamed(core.AppointmentRecurring, 0x000b).property:
		m.Recurring = bool(value.value.(core.Bool))
	case calendarSearchNamed(appointmentRecur, 0x0102).property:
		m.Recurring = len(value.value.(core.Binary)) != 0
	case calendarSearchNamed(appointmentSequence, 0x0003).property:
		m.Sequence = int32(value.value.(core.Integer))
	case fixedContentsProperty(meetingResponseRequestedTag):
		requested := bool(value.value.(core.Bool))
		m.ResponseRequested = &requested
	default:
		return 0, false
	}
	return 0, true
}

func (d *mailDraft) loadMeeting(ctx context.Context) error {
	ref := mailmime.DraftReference(d.staged.MIME)
	if ref == "" {
		return nil
	}
	backing, ok := d.store.durable.store.(meetingDraftStore)
	if !ok {
		return nil
	}
	fields, err := backing.DraftProperties(ctx, d.store.durable.mailbox, ref)
	if err != nil {
		return err
	}
	if raw := fields[meetingDraftProperty]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &d.meeting); err != nil || d.meeting.Class == "" {
			return core.ErrInvalid
		}
	}
	return nil
}

func (d *mailDraft) persistMeeting(ctx context.Context) error {
	backing, ok := d.store.durable.store.(meetingDraftStore)
	if !ok {
		return core.ErrUnsupported
	}
	raw, err := json.Marshal(d.meeting)
	if err != nil {
		return core.ErrInvalid
	}
	mailbox := d.store.durable.mailbox
	data := core.Text(raw)
	if d.staged.ID == "" {
		return backing.SetDraftStringProperty(ctx, mailbox, d.ref, meetingDraftProperty, data)
	}
	// Publish a new opaque reference with the new MIME. A concurrent read or
	// rejected SOAP update keeps the preceding MIME and its own metadata.
	ref, err := backing.RegisterDraftProperties(ctx, mailbox, map[core.Key]core.Text{meetingDraftProperty: data})
	if err != nil {
		return err
	}
	raw = core.StripDraftReferenceHeader(d.staged.MIME)
	d.staged.MIME = append([]byte(core.DraftReferenceHeader+": "+ref+"\r\n"), raw...)
	d.staged.Properties[core.MessageSize] = core.Integer(len(d.staged.MIME))
	return nil
}

func (m meetingMailDraft) properties() core.Properties {
	p := core.Properties{
		core.MeetingMessageType:     core.Integer(draftMeetingKind(m.Class)),
		core.MeetingMessageUID:      core.Text(m.UID),
		core.MeetingMessageAllDay:   core.Bool(m.AllDay),
		core.MeetingMessageSequence: core.Integer(m.Sequence),
	}
	if m.Location != "" {
		p[core.MeetingMessageLocation] = core.Text(m.Location)
	}
	if !m.Start.IsZero() {
		p[core.MeetingMessageStart] = core.Timestamp(m.Start)
	}
	if !m.End.IsZero() {
		p[core.MeetingMessageEnd] = core.Timestamp(m.End)
	}
	if m.ResponseRequested != nil {
		p[core.MeetingResponseRequested] = core.Bool(*m.ResponseRequested)
	}
	return p
}

func (d *mailDraft) submitMeeting(ctx context.Context, session core.Session, native core.Object) error {
	m := d.meeting
	kind := draftMeetingKind(m.Class)
	journal, durable := d.store.durable.store.(core.MeetingSendJournal)
	if !durable || kind == 0 || m.UID == "" || m.InvalidIdentity || m.Recurring || m.Sequence < 0 ||
		native.Properties[core.HasAttachments] == core.Bool(true) {
		return core.ErrUnsupported
	}
	box := session.Mailbox()
	folders, ok := box.(core.SpecialFolderReader)
	if !ok {
		return core.ErrUnsupported
	}
	calendar, err := folders.SpecialFolder(ctx, core.CalendarFolder)
	if err != nil {
		return err
	}
	if calendar.ID == "" || calendar.Kind != core.Folder {
		return core.ErrInvalid
	}
	projected, err := projectMailContents(native)
	if err != nil {
		return err
	}
	if len(projected.attachments) != 0 {
		return core.ErrUnsupported
	}
	plain, _ := projected.values[fixedContentsProperty(0x1000001f)].(core.Text)
	body, rich := string(plain), false
	if raw, ok := projected.values[fixedContentsProperty(0x10130102)].(core.Binary); ok {
		cp, _ := projected.values[fixedContentsProperty(0x3fde0003)].(core.Integer)
		charset := restrictionCharset(uint32(cp))
		if charset == "" {
			return core.ErrUnsupported
		}
		body, err = decodeMailText(charset, raw)
		if err != nil {
			return err
		}
		rich = true
	}
	subject, _ := native.Properties[core.Subject].(core.Text)
	attendees := make([]core.MeetingAttendee, 0, len(native.Recipients))
	for _, recipient := range native.Recipients {
		if recipient.Kind != core.RecipientTo && recipient.Kind != core.RecipientCc {
			return core.ErrInvalidSendRecipients
		}
		attendees = append(attendees, core.MeetingAttendee{Address: recipient.Address, Name: recipient.DisplayName, Optional: recipient.Kind == core.RecipientCc})
	}
	slices.SortFunc(attendees, func(a, b core.MeetingAttendee) int {
		return strings.Compare(strings.ToLower(a.Address), strings.ToLower(b.Address))
	})
	request := core.MeetingInvitation{
		Appointment: core.Appointment{Type: core.AppointmentSingle, Start: m.Start.UTC(), End: m.End.UTC(),
			AllDay: m.AllDay, Subject: string(subject), Location: m.Location, Body: body},
		Attendees: attendees, HTML: rich, RequestResponses: m.ResponseRequested == nil || *m.ResponseRequested,
	}
	// The request fingerprint stays constant across handle reopen and draft
	// revisions. Native send acceptance is independent of a Sent copy.
	details, err := json.Marshal(request)
	if err != nil {
		return core.ErrInvalid
	}
	keyData, err := json.Marshal(struct {
		Kind     core.MeetingMessageKind
		UID      string
		Sequence int32
	}{kind, m.UID, m.Sequence})
	if err != nil {
		return core.ErrInvalid
	}
	key := sha256.Sum256(append([]byte("MAPI meeting submission\x00"), keyData...))
	if kind == core.MeetingRequestMessage {
		if m.Sequence != 0 {
			return core.ErrUnsupported
		}
		key = core.MeetingInvitationKey(m.UID)
	}
	payload := sha256.Sum256(details)
	_, err = core.SubmitMeetingRequest(ctx, journal, d.store.durable.mailbox, key, payload,
		func(ctx context.Context) (core.SendAttempt, error) {
			return stageMAPIMeeting(ctx, box, calendar.ID, m.UID, kind, m.Sequence, request)
		})
	return err
}

func stageMAPIMeeting(ctx context.Context, box core.Mailbox, calendar core.ID, uid string,
	kind core.MeetingMessageKind, sequence int32, request core.MeetingInvitation) (core.SendAttempt, error) {
	if kind == core.MeetingRequestMessage {
		backend, ok := box.(core.MeetingInvitationBackend)
		if !ok || sequence != 0 || !request.Appointment.Start.Before(request.Appointment.End) {
			return nil, core.ErrUnsupported
		}
		return backend.StageMeetingInvitation(calendar, request, uid)
	}
	reader, ok := box.(core.MeetingAppointmentReader)
	if !ok {
		return nil, core.ErrUnsupported
	}
	item, err := reader.GetMeetingByUID(ctx, calendar, uid)
	if err != nil {
		return nil, err
	}
	if item.Meeting == nil || item.UID != uid || item.Identity.Parent != calendar || item.Identity.Kind != core.AppointmentItem {
		return nil, core.ErrConflict
	}
	participants := item.Meeting.Participants
	if kind == core.MeetingCancellationMessage {
		backend, ok := box.(core.MeetingCancellationBackend)
		if !ok || !item.Meeting.OwnerOrganizer {
			return nil, core.ErrUnsupported
		}
		// MS-OXOCAL allows the client to increment the sequence in the
		// cancellation. Zimbra advances it when committing the native action.
		if seq := int64(sequence); seq < int64(item.Meeting.Sequence) || seq > int64(item.Meeting.Sequence)+1 {
			return nil, core.ErrConflict
		}
		seen := make(map[string]bool, len(participants))
		for _, member := range participants {
			seen[strings.ToLower(member.Address)] = true
		}
		if len(seen) != len(participants) {
			return nil, core.ErrInvalidSendRecipients
		}
		// Outlook can include the organizer in the cancellation draft.
		// Zimbra sends to the native participants using the calendar item.
		organizer := strings.ToLower(item.Meeting.Organizer.Address)
		organizerIncluded := seen[organizer]
		for _, recipient := range request.Attendees {
			key := strings.ToLower(recipient.Address)
			if !seen[key] {
				if key == organizer && key != "" && !organizerIncluded {
					organizerIncluded = true
					continue
				}
				return nil, core.ErrInvalidSendRecipients
			}
			delete(seen, key)
		}
		if len(seen) != 0 {
			return nil, core.ErrInvalidSendRecipients
		}
		return backend.StageMeetingCancellation(ctx, item, core.MeetingCancellation{Body: request.Appointment.Body, HTML: request.HTML})
	}
	backend, ok := box.(core.MeetingReplyBackend)
	if !ok || item.Meeting.OwnerOrganizer || len(request.Attendees) != 1 ||
		!strings.EqualFold(request.Attendees[0].Address, item.Meeting.Organizer.Address) {
		return nil, core.ErrInvalidSendRecipients
	}
	if uint32(sequence) != item.Meeting.Sequence {
		return nil, core.ErrConflict
	}
	var choice core.MeetingReplyChoice
	switch kind {
	case core.MeetingAcceptedMessage:
		choice = core.MeetingAccept
	case core.MeetingTentativeMessage:
		choice = core.MeetingTentative
	case core.MeetingDeclinedMessage:
		choice = core.MeetingDecline
	default:
		return nil, core.ErrUnsupported
	}
	return backend.StageMeetingReply(ctx, item.Identity, core.MeetingReply{Choice: choice, NotifyOrganizer: true})
}

func (d *mailDraft) meetingObject() core.Object {
	object := d.staged
	if d.meeting.Class != "" {
		object.Properties = maps.Clone(object.Properties)
		maps.Copy(object.Properties, d.meeting.properties())
	}
	return object
}
