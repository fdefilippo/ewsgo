// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"net/mail"
	"strings"

	"ewsgo/internal/core"
)

var (
	appointmentSequence = core.Key{Set: appointmentPropertySet, ID: 0x8201}
	appointmentLastSent = core.Key{Set: appointmentPropertySet, ID: 0x8203}
	appointmentInvited  = core.Key{Set: appointmentPropertySet, ID: 0x8229}
	appointmentResponse = core.Key{Set: appointmentPropertySet, ID: 0x8218}
	meetingClass        = core.Key{Set: meetingPropertySet, ID: 0x0024}
	meetingType         = core.Key{Set: meetingPropertySet, ID: 0x0026}
	serverProcessed     = core.Key{Set: "11000e07-b51b-40d6-af21-caa85edab1d0", ID: 0x85cc}
	serverProcessing    = core.Key{Set: serverProcessed.Set, ID: 0x85cd}
)

var meetingNamedProperties = []calendarNamedProperty{
	calendarSearchNamed(meetingClass, 0x001f),
	calendarSearchNamed(meetingType, 0x0003),
	calendarSearchNamed(serverProcessed, 0x000b),
	calendarSearchNamed(serverProcessing, 0x0003),
}

const (
	meetingResponseRequestedTag = 0x0063000b
	meetingReplyRequestedTag    = 0x0c17000b
	messageToMeTag              = 0x0057000b
	messageCcMeTag              = 0x0058000b
	messageRecipMeTag           = 0x0059000b
)

var meetingDeliveryTags = [...]struct{ receivedBy, representing uint32 }{
	{0x003f0102, 0x00430102}, // EntryId
	{0x0040001f, 0x0044001f}, // Name
	{0x0075001f, 0x0077001f}, // AddressType
	{0x0076001f, 0x0078001f}, // EmailAddress (EX legacy DN)
	{0x00510102, 0x00520102}, // SearchKey
}

var meetingRecipientColumns = [...]uint32{
	0x0fff0102, // PidTagEntryId.
	0x5ff70102, // PidTagRecipientEntryId.
	0x39000003, // PidTagDisplayType.
	0x5ff6001f, // PidTagRecipientDisplayName.
	0x5ffd0003, // PidTagRecipientFlags.
	0x0e0f000b, // PidTagResponsibility.
	0x300b0102, // PidTagSearchKey.
	0x39fe001f, // PidTagSmtpAddress.
}

func smtpSearchKey(address string) core.Binary {
	return core.Binary(strings.ToUpper("SMTP:"+address) + "\x00")
}

func meetingRecipientValues(recipient core.Recipient, organizer bool) [len(meetingRecipientColumns)]core.Value {
	name := recipient.DisplayName
	if name == "" {
		name = recipient.Address
	}
	entry := smtpOneOffEntry(name, recipient.Address)
	flags := core.Integer(1)
	if organizer {
		flags = 3
	}
	// MS-OXCICAL defines the entry IDs and flags for iCalendar recipients.
	return [...]core.Value{entry, entry, core.Integer(0), core.Text(name), flags,
		core.Bool(true), smtpSearchKey(recipient.Address), core.Text(recipient.Address)}
}

func hasCalendarRecipientRows(row messageRow) bool {
	flags, _ := row.object.Properties[appointmentStateFlags].(core.Integer)
	return row.special == core.CalendarFolder && flags&1 != 0 && len(row.object.Recipients) != 0
}

func nativeMeetingKind(p core.Properties) core.MeetingMessageKind {
	kind, _ := p[core.MeetingMessageType].(core.Integer)
	if kind < core.Integer(core.MeetingRequestMessage) || kind > core.Integer(core.MeetingDeclinedMessage) {
		return 0
	}
	return core.MeetingMessageKind(kind)
}

func mailMessageClass(p core.Properties) core.Text {
	switch nativeMeetingKind(p) {
	case core.MeetingRequestMessage:
		return "IPM.Schedule.Meeting.Request"
	case core.MeetingCancellationMessage:
		return "IPM.Schedule.Meeting.Canceled"
	case core.MeetingAcceptedMessage:
		return "IPM.Schedule.Meeting.Resp.Pos"
	case core.MeetingTentativeMessage:
		return "IPM.Schedule.Meeting.Resp.Tent"
	case core.MeetingDeclinedMessage:
		return "IPM.Schedule.Meeting.Resp.Neg"
	default:
		return "IPM.Note"
	}
}

func projectMeetingProperties(values contentsValues, p core.Properties) {
	kind := nativeMeetingKind(p)
	if kind == 0 {
		return
	}
	put := func(key core.Key, typ uint16, value core.Value) {
		if contentsValueType(typ, value) {
			values[calendarSearchNamed(key, typ).property] = value
		}
	}
	if uid, ok := p[core.MeetingMessageUID].(core.Text); ok {
		if id := appointmentGlobalIdentity(string(uid)); id != nil {
			put(appointmentGlobalID, 0x0102, id)
			put(appointmentCleanID, 0x0102, id)
		}
	}
	put(meetingClass, 0x001f, core.Text("IPM.Appointment"))
	if address, ok := p[core.FromAddress].(core.Text); ok && len(address) <= 320 {
		raw := string(address)
		if parsed, err := mail.ParseAddress(raw); err == nil && parsed.Address == raw && validMailText(raw) {
			for i, value := range []string{raw, "SMTP", raw, raw} {
				putContentsValue(values, representingNameTags[i], core.Text(value))
				putContentsValue(values, senderNameTags[i], core.Text(value))
			}
			putContentsValue(values, representingEntryTag, smtpOneOffEntry(raw, raw))
			putContentsValue(values, senderEntryTag, smtpOneOffEntry(raw, raw))
		}
	}
	if value, ok := p[core.MeetingMessageStart].(core.Timestamp); ok {
		putContentsValue(values, appointmentStartDateTag, value)
		put(appointmentStartWhole, 0x0040, value)
		put(appointmentStart, 0x0040, value)
	}
	if value, ok := p[core.MeetingMessageEnd].(core.Timestamp); ok {
		putContentsValue(values, appointmentEndDateTag, value)
		put(appointmentEndWhole, 0x0040, value)
		put(appointmentEnd, 0x0040, value)
	}
	if value, ok := p[core.MeetingMessageLocation].(core.Text); ok && validMailText(string(value)) {
		put(appointmentLocation, 0x001f, value)
	}
	put(appointmentAllDay, 0x000b, p[core.MeetingMessageAllDay])
	put(appointmentSequence, 0x0003, p[core.MeetingMessageSequence])
	switch kind {
	case core.MeetingRequestMessage:
		put(appointmentStateFlags, 0x0003, core.Integer(3))
		put(appointmentResponse, 0x0003, core.Integer(5))
		put(appointmentInvited, 0x000b, core.Bool(true))
		requestType := core.Integer(1)
		if sequence, ok := p[core.MeetingMessageSequence].(core.Integer); ok && sequence > 0 {
			requestType = 0x00010000
		}
		put(meetingType, 0x0003, requestType)
		if value, ok := p[core.MeetingResponseRequested].(core.Bool); ok {
			putContentsValue(values, meetingResponseRequestedTag, value)
			putContentsValue(values, meetingReplyRequestedTag, value)
		}
	case core.MeetingCancellationMessage:
		put(appointmentStateFlags, 0x0003, core.Integer(7))
	case core.MeetingAcceptedMessage:
		put(appointmentResponse, 0x0003, core.Integer(3))
	case core.MeetingTentativeMessage:
		put(appointmentResponse, 0x0003, core.Integer(2))
	case core.MeetingDeclinedMessage:
		put(appointmentResponse, 0x0003, core.Integer(4))
	}
}

// Direct delivery to the authenticated mailbox carries the same EX identity
// in ReceivedBy and ReceivedRepresenting. Aliases and delegation need separate
// delivery evidence.
func (s *ramStore) projectMeetingDelivery(values contentsValues, object core.Object, identity core.Identity) int {
	if s.allocator == nil || s.scope != identity.Scope ||
		nativeMeetingKind(object.Properties) != core.MeetingRequestMessage ||
		object.Properties[core.IsDraft] == core.Bool(true) {
		return 0
	}
	sender, _ := object.Properties[core.FromAddress].(core.Text)
	if sender == "" || strings.EqualFold(string(sender), identity.Name) {
		return 0
	}
	var to, cc, bcc bool
	name := identity.Name
	for _, recipient := range object.Recipients {
		if !strings.EqualFold(recipient.Address, identity.Name) {
			continue
		}
		switch recipient.Kind {
		case core.RecipientTo:
			to = true
		case core.RecipientCc:
			cc = true
		case core.RecipientBcc:
			bcc = true
		default:
			continue
		}
		if recipient.DisplayName != "" && len(recipient.DisplayName) <= 320 && validMailText(recipient.DisplayName) {
			name = recipient.DisplayName
		}
	}
	if !to && !cc && !bcc {
		return 0
	}
	ns := s.allocator.namespace
	mapped, err := ns.Mailbox(identity)
	if err != nil {
		return 0
	}
	entry, err := ns.mailUserEntryID(identity)
	if err != nil {
		return 0
	}
	key := core.Binary(strings.ToUpper("EX:"+mapped.LegacyDN) + "\x00")
	for i, value := range [...]core.Value{core.Binary(entry), core.Text(name), core.Text("EX"), core.Text(mapped.LegacyDN), key} {
		tags := meetingDeliveryTags[i]
		putContentsValue(values, tags.receivedBy, value)
		putContentsValue(values, tags.representing, value)
	}
	putContentsValue(values, messageToMeTag, core.Bool(to))
	putContentsValue(values, messageCcMeTag, core.Bool(cc))
	putContentsValue(values, messageRecipMeTag, core.Bool(true))
	size := 13*64 + 2*(len(entry)+len(name)+len(mapped.LegacyDN)+len(key)+len("EX"))
	if object.Properties[core.MeetingCalendarProcessed] == core.Bool(true) {
		// MS-OXOCAL 3.1.4.7.2.2 records creation and update together.
		const createdOnPrincipal, updatedCalItem = 0x10, 0x80
		values[calendarSearchNamed(serverProcessed, 0x000b).property] = core.Bool(true)
		values[calendarSearchNamed(serverProcessing, 0x0003).property] = core.Integer(createdOnPrincipal | updatedCalItem)
		size += 2 * 64
	}
	return size
}

func registerMeetingNames(ctx context.Context, names *namedPropertyRegistry) error {
	if err := registerCalendarNames(ctx, names); err != nil {
		return err
	}
	return registerCalendarNamedProperties(ctx, names, meetingNamedProperties)
}

func openedMeetingProperty(values contentsValues, tag uint32, names *namedPropertyRegistry, request specificProperties, cp uint32) (folderPropertyValue, bool) {
	property, err := resolveContentsIdentity(tag, names)
	if err != nil {
		return folderPropertyValue{}, false
	}
	for canonical, value := range values {
		if canonical.id == property.id && canonical.name == property.name {
			return calendarPropertyValue(property, canonical, value, tag, names, request, cp), true
		}
	}
	return folderPropertyValue{}, false
}
