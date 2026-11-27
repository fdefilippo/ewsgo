// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestAppointmentImportSetPreservesNamedWireValues(t *testing.T) {
	start := core.Timestamp(time.Date(2027, 3, 29, 9, 0, 0, 0, time.UTC))
	sent := faiSetRequest{
		{0x80000040, start},
		{0x81120003, core.Integer(30)},
		{0x8113000b, core.Bool(false)},
		{0x81140102, core.Binary{1, 2, 3}},
		{0x8115001f, core.Text("Synthetic location")},
		{appointmentStartDateTag, start},
		{0x0002000b, core.Bool(true)}, // Unmapped fixed property.
	}
	wire := faiSetOperation(3, sent)
	r := reader{data: wire[3:]}
	got, err := parseMailImportSet(&r)
	sent[len(sent)-1].value = nil
	if err != nil || r.done() != nil || !reflect.DeepEqual(got, sent) {
		t.Fatal("appointment import decoding did not preserve the supported value types")
	}
}

func TestSingleAppointmentImportRequiresConsistentTimesAndPersonalState(t *testing.T) {
	start := time.Date(2027, 3, 29, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	values := contentsValues{
		fixedContentsProperty(0x001a001f):                           core.Text("IPM.Appointment"),
		fixedContentsProperty(0x0037001f):                           core.Text("Fixture"),
		calendarSearchNamed(appointmentStartWhole, 0x0040).property: core.Timestamp(start),
		calendarSearchNamed(appointmentEndWhole, 0x0040).property:   core.Timestamp(end),
		calendarSearchNamed(appointmentLocation, 0x001f).property:   core.Text("Room D"),
	}
	item, err := singleAppointmentImport(values)
	if err != nil || !item.Start.Equal(start) || !item.End.Equal(end) || item.Location != "Room D" || item.Subject != "Fixture" {
		t.Fatal("single appointment was not decoded", err)
	}
	for _, tc := range []struct {
		property contentsProperty
		value    core.Value
		err      error
	}{
		{fixedContentsProperty(appointmentEndDateTag), core.Timestamp(start), core.ErrInvalid},
		{calendarSearchNamed(appointmentStateFlags, 0x0003).property, core.Integer(1), core.ErrUnsupported},
		{calendarSearchNamed(appointmentAllDay, 0x000b).property, core.Bool(true), core.ErrUnsupported},
		{calendarSearchNamed(core.AppointmentRecurring, 0x000b).property, core.Bool(true), core.ErrUnsupported},
		{calendarSearchNamed(core.ReminderSet, 0x000b).property, core.Bool(true), core.ErrUnsupported},
	} {
		values[tc.property] = tc.value
		_, err := singleAppointmentImport(values)
		if !errors.Is(err, tc.err) {
			t.Errorf("import returned %v, want %v", err, tc.err)
		}
		delete(values, tc.property)
	}
	delete(values, calendarSearchNamed(appointmentStartWhole, 0x0040).property)
	delete(values, calendarSearchNamed(appointmentEndWhole, 0x0040).property)
	values[fixedContentsProperty(appointmentStartDateTag)] = core.Timestamp(start)
	values[calendarSearchNamed(appointmentEnd, 0x0040).property] = core.Timestamp(end)
	item, err = singleAppointmentImport(values)
	if err != nil || !item.Start.Equal(start) || !item.End.Equal(end) {
		t.Fatal("could not reconcile equivalent dates in a partial appointment upload", err)
	}
	values[calendarSearchNamed(appointmentStart, 0x0040).property] = core.Timestamp(end)
	if _, err := singleAppointmentImport(values); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("contradictory date aliases were accepted", err)
	}
}

func appointmentImportRomeZone() *core.AppointmentZone {
	return &core.AppointmentZone{
		Name: "W. Europe Standard Time", StandardOffset: 60, DaylightOffset: 120,
		Standard: core.AppointmentZoneTransition{Month: time.October, Week: 5, Weekday: time.Sunday, Hour: 3},
		Daylight: core.AppointmentZoneTransition{Month: time.March, Week: 5, Weekday: time.Sunday, Hour: 2},
	}
}

func TestAppointmentImportAllDayUsesEffectiveStartZone(t *testing.T) {
	raw, ok := encodeAppointmentZoneDefinition(appointmentImportRomeZone(), false)
	if !ok {
		t.Fatal("could not encode test zone")
	}
	start := time.Date(2027, 3, 27, 23, 0, 0, 0, time.UTC)
	end := time.Date(2027, 3, 28, 22, 0, 0, 0, time.UTC)
	values := contentsValues{
		fixedContentsProperty(0x001a001f):                          core.Text("IPM.Appointment"),
		fixedContentsProperty(appointmentStartDateTag):             core.Timestamp(start),
		fixedContentsProperty(appointmentEndDateTag):               core.Timestamp(end),
		calendarSearchNamed(appointmentAllDay, 0x000b).property:    core.Bool(true),
		calendarSearchNamed(appointmentStartZone, 0x0102).property: raw,
		calendarSearchNamed(appointmentDuration, 0x0003).property:  core.Integer(23 * 60),
	}
	expected := time.Date(2027, 3, 28, 0, 0, 0, 0, time.UTC)
	durationKey := calendarSearchNamed(appointmentDuration, 0x0003).property
	for _, minutes := range []core.Integer{23 * 60, 24 * 60} {
		values[durationKey] = minutes
		item, err := singleAppointmentImport(values)
		if err != nil || !item.AllDay || !item.Start.Equal(expected) || !item.End.Equal(expected.AddDate(0, 0, 1)) {
			t.Fatal("daylight shift changed the civil appointment dates", err)
		}
	}
	values[durationKey] = core.Integer(25 * 60)
	if _, err := singleAppointmentImport(values); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("unrelated duration was accepted", err)
	}
	zoneKey := calendarSearchNamed(appointmentStartZone, 0x0102).property
	for _, malformed := range []core.Binary{raw[:8], raw[:len(raw)-1], append(bytes.Clone(raw), 0)} {
		values[zoneKey] = malformed
		if _, err := singleAppointmentImport(values); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("malformed zone definition was accepted", err)
		}
	}
	values[zoneKey] = raw
	values[fixedContentsProperty(appointmentStartDateTag)] = core.Timestamp(start.Add(time.Hour))
	delete(values, durationKey)
	if _, err := singleAppointmentImport(values); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("non-midnight start was accepted as all-day", err)
	}
}

func qualifyNativeAppointmentImport(t *testing.T, ctx context.Context, native core.Session, inbox uint64, okROP func([]byte) []byte, run func([]byte, uint32) []byte, direct, allDay bool) {
	t.Helper()
	box := native.Mailbox()
	folder, err := box.(core.SpecialFolderReader).SpecialFolder(ctx, core.CalendarFolder)
	if err != nil {
		t.Fatal("native Calendar unavailable")
	}
	reader, writer := box.(core.AppointmentContentsReader), box.(core.AppointmentWriter)
	title := "EWSGo synthetic appointment upload " + time.Now().UTC().Format("20060102T150405.000000000")
	find := func(ctx context.Context) (matches []core.Appointment) {
		t.Helper()
		items, err := reader.ListAppointments(ctx, folder.ID)
		if err != nil {
			t.Error("calendar fixture lookup failed")
		}
		for _, item := range items {
			if item.Subject == title {
				matches = append(matches, item)
			}
		}
		return matches
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, item := range find(cleanup) {
			if err := writer.TrashAppointment(cleanup, item); err != nil {
				t.Error("could not trash synthetic appointment")
			}
		}
	}()
	okROP(folderOperation(inbox, 4, 0, 1))
	entry := okROP(folderPropertiesOperation(4, 1, 1, 0x36d00102))
	if len(entry) != 55 {
		t.Fatal("Calendar EntryID unavailable")
	}
	fid := binary.LittleEndian.Uint64(okROP(conversionOperation(0x44, entry[31:]))[6:])
	okROP(folderOperation(fid, 4, 0, 1))
	reserved := okROP([]byte{0x7f, 4, 0, 1, 0, 0, 0})
	request := syntheticFAIImport()
	request.flags = 0
	copy(request.source[:], reserved[6:])
	wantMID := folderID(ownMoveCounter([16]byte(request.source[:16]), request.source))
	okROP([]byte{0x7e, 4, 1, 2, 1})
	if reply := okROP(faiImportOperation(request)); len(reply) != 14 || binary.LittleEndian.Uint64(reply[6:]) != 0 {
		t.Fatal("ImportMessageChange should stage an empty appointment")
	}
	start := time.Date(2027, 3, 29, 9, 0, 0, 0, time.UTC)
	end := start.Add(45 * time.Minute)
	civilStart, civilEnd := start, end
	if allDay {
		start = time.Date(2027, 3, 27, 23, 0, 0, 0, time.UTC)
		end = time.Date(2027, 3, 28, 22, 0, 0, 0, time.UTC)
		civilStart = time.Date(2027, 3, 28, 0, 0, 0, 0, time.UTC)
		civilEnd = civilStart.AddDate(0, 0, 1)
	}
	const location, body = "Synthetic fixture", "Appointment text from MAPI"
	if direct {
		names := []propertyName{calendarSearchNamed(appointmentEnd, 0x0040).property.name,
			calendarSearchNamed(appointmentLocation, 0x001f).property.name}
		if allDay {
			names = append(names, calendarSearchNamed(appointmentAllDay, 0x000b).property.name,
				calendarSearchNamed(appointmentStartZone, 0x0102).property.name,
				calendarSearchNamed(appointmentDuration, 0x0003).property.name)
		}
		ids := propertyResponseIDs(t, ROPBuffer{Operations: okROP(propertyIDsOperation(4, 3, 2, names...))})
		values := faiSetRequest{
			{0x001a001f, core.Text("IPM.Appointment")},
			{0x003d001f, core.Text("EWSGo ")}, {0x0e1d001f, core.Text(strings.TrimPrefix(title, "EWSGo "))},
			{appointmentBodyTag, core.Text(body)}, {appointmentStartDateTag, core.Timestamp(start)},
			{uint32(ids[0])<<16 | 0x0040, core.Timestamp(end)},
			{uint32(ids[1])<<16 | 0x001f, core.Text(location)},
		}
		if allDay {
			zone, ok := encodeAppointmentZoneDefinition(appointmentImportRomeZone(), false)
			if !ok {
				t.Fatal("could not encode all-day fixture's display zone")
			}
			values = append(values, faiWireProperty{uint32(ids[2])<<16 | 0x000b, core.Bool(true)},
				faiWireProperty{uint32(ids[3])<<16 | 0x0102, zone},
				faiWireProperty{uint32(ids[4])<<16 | 0x0003, core.Integer(minutesPerDay)})
		}
		if reply := okROP(faiSetOperation(3, values)); !bytes.Equal(reply, propertyProblemsResponse(0x0a, 3, nil)) {
			t.Fatal("direct appointment properties were not staged")
		}
	} else {
		okROP([]byte{ropFastTransferDestinationConfigure, 4, 3, 4, 1, 0})
		fx := &fastTransfer{limit: maxContactEditBytes}
		fx.scalar(0x001a001f, core.Text("IPM.Appointment"))
		fx.scalar(0x0037001f, core.Text(title))
		fx.scalar(appointmentBodyTag, core.Text(body))
		for i, field := range []struct {
			key   core.Key
			kind  uint16
			value core.Value
		}{
			{appointmentStartWhole, 0x0040, core.Timestamp(start)},
			{appointmentEndWhole, 0x0040, core.Timestamp(end)},
			{appointmentLocation, 0x001f, core.Text(location)},
		} {
			p := calendarSearchNamed(field.key, field.kind).property.name
			tag := (uint32(0x8001+i) << 16) | uint32(field.kind)
			fx.word(tag)
			fx.append(p.Set[:], false)
			fx.append([]byte{0}, false)
			fx.word(p.LID)
			fx.scalarValue(tag, field.value)
		}
		if fx.err != nil {
			t.Fatal("failed to encode appointment fixture", fx.err)
		}
		okROP(faiUploadBuffer(4, fx.data[:3]))
		okROP(faiUploadBuffer(4, fx.data[3:]))
		okROP([]byte{1, 4, 4})
	}
	if len(find(ctx)) != 0 {
		t.Fatal("staged appointment reached Zimbra before SaveChanges")
	}
	save := []byte{0x0c, 4, 4, 3, 0}
	saved := okROP(save)
	if len(saved) != 15 || binary.LittleEndian.Uint64(saved[7:]) != wantMID || !bytes.Equal(okROP(save), saved) {
		t.Fatal("appointment save did not return the same client-reserved MID")
	}
	items := find(ctx)
	if len(items) != 1 || items[0].Participation != core.AppointmentPersonal || items[0].Location != location ||
		items[0].Body != body || items[0].AllDay != allDay || !items[0].Start.Equal(civilStart) || !items[0].End.Equal(civilEnd) {
		t.Fatal("native appointment differs from uploaded fixture")
	}
	okROP([]byte{1, 4, 3})
	if reply := run(faiImportOperation(request), 65535); !bytes.Equal(reply, ropError(0x72, 3, faiError(errFAIIgnore, false))) {
		t.Fatal("confirmed upload was not ignored on replay")
	}
	okROP(openMessageOperation(4, 1, 3, fid, wantMID, 0x0fff, 0))
	if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x65e00102)); !bytes.Equal(wire[7:], append([]byte{22, 0}, request.source[:]...)) {
		t.Fatal("opened native appointment lost the client-reserved source key")
	}
	if direct && !allDay {
		okROP([]byte{1, 4, 3})
		edit := request
		edit.change = testIncrementChangeXID(request.change)
		edit.predecessors = faiPCL{edit.change.namespace(): edit.change.local()}
		edit.modified = request.modified.Add(time.Second)
		okROP(faiImportOperation(edit))
		name := calendarSearchNamed(appointmentLocation, 0x001f).property.name
		ids := propertyResponseIDs(t, ROPBuffer{Operations: okROP(propertyIDsOperation(4, 3, 2, name))})
		const updatedLocation = "Synthetic fixture moved"
		newEnd := end.Add(15 * time.Minute)
		values := faiSetRequest{
			{0x001a001f, core.Text("IPM.Appointment")},
			{appointmentStartDateTag, core.Timestamp(start)}, {appointmentEndDateTag, core.Timestamp(newEnd)},
			{uint32(ids[0])<<16 | 0x001f, core.Text(updatedLocation)},
		}
		if reply := okROP(faiSetOperation(3, values)); !bytes.Equal(reply, propertyProblemsResponse(0x0a, 3, nil)) {
			t.Fatal("edit properties were not staged")
		}
		if reply := run(save, 65535); !bytes.Equal(reply, saved) {
			state := find(ctx)
			if len(reply) >= 6 && len(state) == 1 {
				t.Logf("save status 0x%08x, native version changed %t, body intact %t, time updated %t, location updated %t",
					binary.LittleEndian.Uint32(reply[2:]), state[0].Identity.Revision > items[0].Identity.Revision,
					state[0].Body == body, state[0].End.Equal(newEnd), state[0].Location == updatedLocation)
			}
			t.Fatal("edit did not preserve the appointment MID")
		}
		after := find(ctx)
		if len(after) != 1 || after[0].Identity.ID != items[0].Identity.ID || after[0].UID != items[0].UID ||
			after[0].Identity.Revision <= items[0].Identity.Revision || after[0].Body != body ||
			after[0].Location != updatedLocation || !after[0].Start.Equal(start) || !after[0].End.Equal(newEnd) {
			t.Fatal("native edit did not preserve identity and unmodified fields")
		}
		okROP([]byte{1, 4, 3})
		if reply := run(faiImportOperation(edit), 65535); !bytes.Equal(reply, ropError(0x72, 3, faiError(errFAIIgnore, false))) {
			t.Fatal("confirmed edit was not ignored on replay")
		}
		stale := edit
		stale.change = gidChangeXID(faiTestXID(2, 3))
		stale.predecessors = faiPCL{
			request.change.namespace(): request.change.local(),
			stale.change.namespace():   stale.change.local(),
		}
		if reply := run(faiImportOperation(stale), 65535); !bytes.Equal(reply, ropError(0x72, 3, faiError(core.ErrConflict, false))) {
			t.Fatal("stale predecessor list did not report a conflict")
		}
		okROP(openMessageOperation(4, 1, 3, fid, wantMID, 0x0fff, 0))
		if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x65e00102)); !bytes.Equal(wire[7:], append([]byte{22, 0}, request.source[:]...)) {
			t.Fatal("edited native appointment lost its source key")
		}
		okROP([]byte{1, 4, 3})
		const directLocation = "Synthetic fixture directly edited"
		directEnd := newEnd.Add(10 * time.Minute)
		directValues := faiSetRequest{
			{appointmentEndDateTag, core.Timestamp(directEnd)},
			{uint32(ids[0])<<16 | 0x001f, core.Text(directLocation)},
		}
		// Outlook can open in read/write mode and set fields on the new
		// handle within one Execute. No native write is allowed before Save.
		batch := append(openMessageOperation(4, 1, 3, fid, wantMID, 0x0fff, 1|openMessageIncludeSoftDeleted), faiSetOperation(3, directValues)...)
		if reply := run(batch, 65535); len(reply) <= 8 || !bytes.Equal(reply[:6], ropError(3, 3, 0)) ||
			!bytes.Equal(reply[len(reply)-8:], propertyProblemsResponse(0x0a, 3, nil)) {
			t.Fatal("read/write Open and Set in the same batch failed")
		}
		okROP(openMessageOperation(4, 1, 4, fid, wantMID, 0x0fff, 1))
		okROP(faiSetOperation(4, faiSetRequest{{uint32(ids[0])<<16 | 0x001f, core.Text("Synthetic stale edit")}}))
		unchanged := find(ctx)
		if len(unchanged) != 1 || unchanged[0].Identity.Revision != after[0].Identity.Revision {
			t.Fatal("staging the direct edit touched the native appointment")
		}
		if reply := run(save, 65535); !bytes.Equal(reply, saved) {
			state := find(ctx)
			if len(reply) >= 6 && len(state) == 1 {
				t.Logf("direct Save 0x%08x, native version changed %t, time updated %t, location updated %t",
					binary.LittleEndian.Uint32(reply[2:]), state[0].Identity.Revision > after[0].Identity.Revision,
					state[0].End.Equal(directEnd), state[0].Location == directLocation)
			}
			t.Fatal("direct edit failed to preserve MID or confirm save")
		}
		if reply := run([]byte{0x0c, 4, 3, 4, 0}, 65535); !bytes.Equal(reply, ropError(0x0c, 3, faiError(core.ErrConflict, true))) {
			t.Fatal("direct Save accepted a stale native revision")
		}
		updated := find(ctx)
		if len(updated) != 1 || updated[0].Identity.ID != after[0].Identity.ID || updated[0].UID != after[0].UID ||
			updated[0].Identity.Revision <= after[0].Identity.Revision || updated[0].Body != body ||
			updated[0].Location != directLocation || !updated[0].Start.Equal(start) || !updated[0].End.Equal(directEnd) {
			t.Fatal("direct edit did not preserve native identity and untouched fields")
		}
		if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x65e00102)); !bytes.Equal(wire[7:], append([]byte{22, 0}, request.source[:]...)) {
			t.Fatal("direct edit changed the source key")
		}
		okROP([]byte{1, 4, 4})
	}
	for _, slot := range []byte{3, 2, 1} {
		okROP([]byte{1, 4, slot})
	}
}

type appointmentImportMailbox struct {
	*calendarMailbox
	item    core.Appointment
	creates int
}

func (b *appointmentImportMailbox) CreateAppointmentWithUID(_ context.Context, folder core.ID, item core.Appointment, uid string) (core.Appointment, error) {
	b.creates++
	item.UID = uid
	item.Identity = core.Object{ID: "appointment:42", Parent: folder, Kind: core.AppointmentItem, Revision: 7,
		Properties: core.Properties{core.IsRead: core.Bool(true), core.MetadataRevision: core.Integer(7),
			core.ContentRevision: core.Integer(7),
			core.LastModifiedAt:  core.Timestamp(time.Unix(1800000000, 0).UTC())}}
	b.item = item
	return core.Appointment{}, core.ErrOutcomeUnknown
}

func (b *appointmentImportMailbox) FindAppointmentByUID(_ context.Context, folder core.ID, uid string) (core.Appointment, error) {
	if b.item.UID != uid || b.item.Identity.Parent != folder {
		return core.Appointment{}, core.ErrNotFound
	}
	return b.item, nil
}

func (b *appointmentImportMailbox) GetCalendarAppointment(_ context.Context, folder, id core.ID) (core.Appointment, error) {
	if b.item.Identity.ID != id || b.item.Identity.Parent != folder {
		return core.Appointment{}, core.ErrNotFound
	}
	return b.item, nil
}

func (b *appointmentImportMailbox) ListAppointments(_ context.Context, folder core.ID) ([]core.Appointment, error) {
	if b.item.Identity.Parent != folder {
		return nil, nil
	}
	return []core.Appointment{b.item}, nil
}

func TestSQLiteAppointmentImportReconcilesWithoutCreatingAgain(t *testing.T) {
	f, cal := newCalendarFixture(t)
	backend := &appointmentImportMailbox{calendarMailbox: cal}
	f.session = &testSession{box: backend}
	f.transport.EnableFAICollectorSpike()
	f.transport.EnableContentsDownloadSpike()
	f.transport.EnableHierarchyDownloadSpike()
	reopen := sqliteExecuteFixture(t, f)
	db := reopen()
	open := func() uint64 {
		t.Helper()
		f.handles = append(f.handles, 0xffffffff)
		reply := faiRunOK(t, f, conversionOperation(0x44, calendarEntry(t, f)[22:]))
		return binary.LittleEndian.Uint64(reply[6:])
	}
	fid := open()
	s := replicaStore(t, f)
	reserved := faiRunOK(t, f, []byte{0x7f, 4, 0, 1, 0, 0, 0})
	r := syntheticFAIImport()
	r.flags = 0
	copy(r.source[:], reserved[6:])
	counter := ownMoveCounter(s.replica, r.source)
	newDraft := func(s *ramStore) *mailImportDraft {
		t.Helper()
		d, err := s.newMessageImport(t.Context(), f.session, fid, r, nil)
		if err != nil {
			t.Fatal("could not stage an appointment", err)
		}
		return d
	}
	d := newDraft(s)
	start := time.Date(2027, 3, 29, 9, 0, 0, 0, time.UTC)
	item := core.Appointment{Subject: "Fixture", Start: start, End: start.Add(time.Hour), Participation: core.AppointmentPersonal}
	if _, err := s.saveAppointmentImport(t.Context(), f.session, fid, d, item); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("missing native acknowledgement was not preserved", err)
	}
	pending := s.pendingMessageImports[folderID(counter)]
	if pending.Special() != core.CalendarFolder || pending.NativeID != "" || pending.AppointmentReference == ([16]byte{}) || backend.creates != 1 {
		t.Fatal("missing durable appointment claim")
	}
	if _, err := s.observeCalendarSearchItems(t.Context(), f.session, fid, core.CalendarFolder); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("an alternate MID could be assigned to a pending native appointment", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	s = replicaStore(t, f)
	if open() != fid {
		t.Fatal("Calendar FID changed across SQLite restart")
	}
	d = newDraft(s)
	other := item
	other.Subject = "Changed fixture"
	if _, err := s.saveAppointmentImport(t.Context(), f.session, fid, d, other); !errors.Is(err, core.ErrConflict) {
		t.Fatal("changed payload was accepted for an unresolved create", err)
	}
	if _, err := s.saveAppointmentImport(t.Context(), f.session, fid, d, item); err != nil || backend.creates != 1 {
		t.Fatal("reconciliation did not publish the native appointment exactly once", err)
	}
	view, err := s.observeCalendarSearchItems(t.Context(), f.session, fid, core.CalendarFolder)
	if err != nil || len(view.messages) != 1 || view.messages[0].mid != folderID(counter) || view.messages[0].sourceKey != r.source {
		t.Fatal("the published appointment lost the stable Outlook identity", err)
	}
}

func TestAppointmentImportFiniteWeeklyAndException(t *testing.T) {
	first := time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC)
	wall := first.Add(9 * time.Hour)
	start := wall.Add(-time.Hour)
	end := start.Add(time.Hour)
	item := core.Appointment{Type: core.AppointmentMaster, Subject: "Fixture", Location: "Main room", Start: start, End: end,
		Recurrence: &core.AppointmentRecurrence{Frequency: core.RecurrenceWeekly, Interval: 1, Count: 3,
			StartDate: first, Days: []time.Weekday{time.Monday}, FirstDay: time.Monday},
		SeriesSchedule: &core.AppointmentSeriesSchedule{WallStart: wall, WallEnd: wall.Add(time.Hour)}}
	zone, _ := encodeAppointmentZoneDefinition(appointmentImportRomeZone(), true)
	encoded, ok := encodeAppointmentRecurrence(item)
	if !ok {
		t.Fatal("could not encode test recurrence")
	}
	values := contentsValues{
		fixedContentsProperty(0x001a001f):                           core.Text("IPM.Appointment"),
		fixedContentsProperty(0x0037001f):                           core.Text(item.Subject),
		calendarSearchNamed(appointmentLocation, 0x001f).property:   core.Text(item.Location),
		calendarSearchNamed(appointmentStartWhole, 0x0040).property: core.Timestamp(start),
		calendarSearchNamed(appointmentEndWhole, 0x0040).property:   core.Timestamp(end),
		calendarSearchNamed(appointmentRecurZone, 0x0102).property:  zone,
		calendarSearchNamed(appointmentRecurType, 0x0003).property:  core.Integer(encoded.kind),
		appointmentRecurrenceProperty:                               encoded.data,
	}
	got, err := appointmentImportItem(values)
	if err != nil || !equalAppointmentRules(got.Recurrence, item.Recurrence) || got.TimeZone != "Europe/Rome" ||
		!got.SeriesSchedule.WallStart.Equal(wall) {
		t.Fatal("could not create the weekly series", err)
	}
	// The effective TZDEFINITION rule can lack the recurrence flag.
	// The legacy recurrence structure still establishes the same offsets.
	effective, _ := encodeAppointmentZoneDefinition(appointmentImportRomeZone(), false)
	zoneStruct := calendarSearchNamed(appointmentZoneStruct, 0x0102).property
	values[zoneStruct] = encodeAppointmentZoneStruct(*appointmentImportRomeZone())
	values[calendarSearchNamed(appointmentRecurZone, 0x0102).property] = effective
	got, err = appointmentImportItem(values)
	if err != nil || got.TimeZone != "Europe/Rome" || !got.SeriesSchedule.WallStart.Equal(wall) {
		t.Fatal("could not match the legacy recurrence to its named time zone", err)
	}
	values[zoneStruct] = encodeAppointmentZoneStruct(core.AppointmentZone{})
	if _, err := appointmentImportItem(values); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("a different effective time zone cannot replace the legacy rule", err)
	}
	delete(values, zoneStruct)
	anchor := wall.AddDate(0, 0, 7)
	exception := core.AppointmentSeriesException{WallStart: anchor.Add(time.Hour), WallEnd: anchor.Add(2 * time.Hour),
		WallOriginalStart: anchor, Subject: "Fixture", Location: "Sala città"}
	item.SeriesSchedule.Exceptions = []core.AppointmentSeriesException{exception}
	encoded, ok = encodeAppointmentRecurrence(item)
	if !ok {
		t.Fatal("could not encode the exception")
	}
	data := encoded.data
	values[appointmentRecurrenceProperty] = data
	got, err = appointmentSeriesImport(values, data)
	if err != nil || len(got.SeriesSchedule.Exceptions) != 1 || got.SeriesSchedule.Exceptions[0] != exception {
		t.Fatal("could not decode the original anchor or Unicode location", err)
	}
	if _, err := appointmentImportItem(values); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("creation with unsupported preexisting occurrence changes must fail before writing", err)
	}
	if _, _, err := decodeAppointmentRecurrence(data[:len(data)-1], item); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("truncated recurrence was accepted", err)
	}
	utc, ok := wallAppointmentInstant(anchor, *appointmentImportRomeZone())
	if !ok || !utc.Equal(anchor.Add(-2*time.Hour)) {
		t.Fatal("exception did not use the daylight offset at its anchor")
	}
}

func TestAppointmentClipBounds(t *testing.T) {
	first := time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC)
	wall := first.Add(9 * time.Hour)
	item := core.Appointment{
		Type: core.AppointmentSingle, Start: wall.Add(-time.Hour), End: wall,
		StartZone: appointmentImportRomeZone(),
	}
	check := func(start, end time.Time) {
		t.Helper()
		p := mapAppointmentProperties(item)
		if p[appointmentClipStart] != core.Timestamp(start) || p[appointmentClipEnd] != core.Timestamp(end) {
			t.Fatalf("Clip bounds = %v, %v, want %v, %v", p[appointmentClipStart], p[appointmentClipEnd], start, end)
		}
	}
	check(item.Start, item.End)
	item.Type = core.AppointmentMaster
	item.Recurrence = &core.AppointmentRecurrence{
		Frequency: core.RecurrenceWeekly, Interval: 1, Count: 3,
		StartDate: first, Days: []time.Weekday{time.Monday}, FirstDay: time.Monday,
	}
	item.SeriesSchedule = &core.AppointmentSeriesSchedule{WallStart: wall, WallEnd: wall.Add(time.Hour)}
	check(first.Add(-time.Hour), first.AddDate(0, 0, 14).Add(-2*time.Hour))
	item.Recurrence.Count = 0
	check(first.Add(-time.Hour), time.Date(4500, 8, 31, 23, 59, 0, 0, time.UTC))
	for _, key := range []core.Key{appointmentClipStart, appointmentClipEnd} {
		p := calendarSearchNamed(key, 0x0040).property
		if !tableRestrictionSchema(&contentsTable{special: core.CalendarFolder}, p) ||
			tableRestrictionSchema(&contentsTable{}, p) {
			t.Fatalf("clip property has the wrong table restriction scope: %v", key)
		}
	}
}

func TestCalendarAttachmentTableWithNoAttachments(t *testing.T) {
	f, cal := newCalendarFixture(t)
	f.transport.EnableContentsDownloadSpike()
	f.transport.EnableContentsTableSpike()
	t.Cleanup(f.state.mailbox.close)
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	start := time.Date(2027, time.March, 29, 9, 0, 0, 0, time.UTC)
	box := &appointmentImportMailbox{calendarMailbox: cal, item: core.Appointment{
		Identity: core.Object{ID: "appointment:42", Parent: cal.calendar.ID, Kind: core.AppointmentItem, Revision: 7,
			Properties: core.Properties{core.IsRead: core.Bool(true), core.HasAttachments: core.Bool(false),
				core.MetadataRevision: core.Integer(7), core.ContentRevision: core.Integer(7), core.LastModifiedAt: core.Timestamp(start)}},
		Subject: "Fixture", Start: start, End: start.Add(time.Hour), Participation: core.AppointmentPersonal,
	}}
	f.session = &testSession{box: box}
	converted := faiRunOK(t, f, conversionOperation(0x44, calendarEntry(t, f)[22:]))
	fid := binary.LittleEndian.Uint64(converted[6:])
	faiRunOK(t, f, folderOperation(fid, 4, 0, 1))
	store := replicaStore(t, f)
	view, err := store.observeCalendarSearchItems(t.Context(), f.session, fid, core.CalendarFolder)
	if err != nil || len(view.messages) != 1 {
		t.Fatal("could not observe the calendar appointment", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 3, fid, view.messages[0].mid, 0x0fff, 0))
	faiRunOK(t, f, []byte{0x21, 4, 3, 2, 0})
	if position := faiRunOK(t, f, []byte{0x17, 4, 2}); len(position) != 14 || binary.LittleEndian.Uint32(position[10:]) != 0 {
		t.Fatal("appointment attachment table was not empty")
	}
	box.item.Identity.Properties[core.HasAttachments] = core.Bool(true)
	if wire, _ := f.run([]byte{0x17, 4, 2}); !bytes.Equal(wire, ropError(0x17, 2, NotSupported)) {
		t.Fatal("empty table continued to authorize after attachment appeared")
	}
}

func qualifyNativeRecurringAppointment(t *testing.T, ctx context.Context, native core.Session, inbox uint64, okROP func([]byte) []byte, run func([]byte, uint32) []byte) {
	t.Helper()
	box := native.Mailbox()
	reader := box.(core.AppointmentContentsReader)
	writer := box.(core.AppointmentWriter)
	folder, err := box.(core.SpecialFolderReader).SpecialFolder(ctx, core.CalendarFolder)
	if err != nil {
		t.Fatal("native Calendar unavailable")
	}
	title := "EWSGo recurring fixture " + time.Now().UTC().Format("20060102T150405.000000000")
	read := func(ctx context.Context) core.Appointment {
		t.Helper()
		items, err := reader.ListAppointments(ctx, folder.ID)
		if err != nil {
			t.Fatal("could not read native Calendar", err)
		}
		var result core.Appointment
		for _, item := range items {
			if item.Subject == title {
				if result.Identity.ID != "" {
					t.Fatal("duplicate native fixture")
				}
				result = item
			}
		}
		return result
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if item := read(cleanup); item.Identity.ID != "" {
			if err := writer.TrashAppointment(cleanup, item); err != nil {
				t.Error("could not trash native recurring fixture", err)
			}
		}
	}()

	okROP(folderOperation(inbox, 4, 0, 1))
	entry := okROP(folderPropertiesOperation(4, 1, 1, 0x36d00102))
	fid := binary.LittleEndian.Uint64(okROP(conversionOperation(0x44, entry[31:]))[6:])
	okROP(folderOperation(fid, 4, 0, 1))
	names := []propertyName{
		appointmentRecurrenceProperty.name,
		calendarSearchNamed(appointmentRecurZone, 0x0102).property.name,
		calendarSearchNamed(appointmentLocation, 0x001f).property.name,
	}
	ids := propertyResponseIDs(t, ROPBuffer{Operations: okROP(propertyIDsOperation(4, 1, 2, names...))})
	recurTag, zoneTag, locationTag := uint32(ids[0])<<16|0x0102, uint32(ids[1])<<16|0x0102, uint32(ids[2])<<16|0x001f

	date := time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC)
	wall := date.Add(10 * time.Hour)
	expected := core.Appointment{
		Subject: title, Type: core.AppointmentMaster, Location: "Initial room",
		Start: wall.Add(-time.Hour), End: wall,
		Recurrence: &core.AppointmentRecurrence{
			Frequency: core.RecurrenceWeekly, Interval: 1, Count: 3, StartDate: date,
			Days: []time.Weekday{time.Monday}, FirstDay: time.Sunday,
		},
		SeriesSchedule: &core.AppointmentSeriesSchedule{WallStart: wall, WallEnd: wall.Add(time.Hour)},
	}
	encodeRecurrence := func(item core.Appointment) core.Binary {
		t.Helper()
		encoded, ok := encodeAppointmentRecurrence(item)
		if !ok {
			t.Fatal("could not encode recurring fixture")
		}
		return encoded.data
	}
	zone, ok := encodeAppointmentZoneDefinition(appointmentImportRomeZone(), true)
	if !ok {
		t.Fatal("could not encode recurrence time zone")
	}
	create := binary.LittleEndian.AppendUint64([]byte{0x06, 4, 1, 3, 0xff, 0x0f}, fid)
	create = append(create, 0)
	set := faiSetOperation(3, faiSetRequest{
		{0x0037001f, core.Text(title)}, {locationTag, core.Text(expected.Location)},
		{appointmentStartDateTag, core.Timestamp(expected.Start)}, {appointmentEndDateTag, core.Timestamp(expected.End)},
		{recurTag, encodeRecurrence(expected)}, {zoneTag, zone},
	})
	if reply := okROP(append(create, set...)); len(reply) != 15 || !bytes.Equal(reply[7:], propertyProblemsResponse(0x0a, 3, nil)) {
		t.Fatal("CreateMessage and SetProperties did not stage a series")
	}
	save := []byte{0x0c, 4, 4, 3, 2}
	saved := okROP(save)
	first := read(ctx)
	if first.UID == "" || first.SeriesSchedule == nil || !equalAppointmentRules(first.Recurrence, expected.Recurrence) ||
		!first.Start.Equal(expected.Start) || first.Location != expected.Location {
		t.Fatal("new native series differs")
	}
	const afterLocation = "Edited master room"
	okROP(faiSetOperation(3, faiSetRequest{{locationTag, core.Text(afterLocation)}}))
	if !bytes.Equal(okROP(save), saved) {
		t.Fatal("master edit changed MAPI identity")
	}
	master := read(ctx)
	if master.Location != afterLocation || master.UID != first.UID || master.Identity.Revision <= first.Identity.Revision {
		t.Fatal("native master was not edited in place")
	}
	occurrence := wall.AddDate(0, 0, 7)
	exception := core.AppointmentSeriesException{
		WallOriginalStart: occurrence, WallStart: occurrence.Add(30 * time.Minute), WallEnd: occurrence.Add(90 * time.Minute),
		Subject: title, Location: "Edited occurrence room",
	}
	master.SeriesSchedule.Exceptions = []core.AppointmentSeriesException{exception}
	okROP(faiSetOperation(3, faiSetRequest{{recurTag, encodeRecurrence(master)}}))
	if !bytes.Equal(okROP(save), saved) {
		t.Fatal("exception edit changed MAPI identity")
	}
	after := read(ctx)
	if after.UID != first.UID || after.Location != afterLocation || after.Identity.Revision <= master.Identity.Revision ||
		after.SeriesSchedule == nil || !sameAppointmentExceptions(after.SeriesSchedule.Exceptions, []core.AppointmentSeriesException{exception}) ||
		!equalAppointmentRules(after.Recurrence, expected.Recurrence) {
		t.Fatal("native exception differs or changed the whole series")
	}
	// Outlook in cache uses an embedded calendar message while it imports the
	// recurrence. Saving the embedded objects must leave Zimbra untouched.
	source := okROP(folderPropertiesOperation(4, 3, 1, 0x65e00102))
	change := okROP(folderPropertiesOperation(4, 3, 1, 0x65e20102))
	request := syntheticFAIImport()
	request.flags = 0
	copy(request.source[:], source[9:])
	serverChange := changeXID(change[9:])
	if !serverChange.valid() {
		t.Fatal("could not read the server-generated calendar change key")
	}
	request.predecessors[serverChange.namespace()] = serverChange.local()
	okROP([]byte{1, 4, 3})
	okROP([]byte{0x7e, 4, 1, 2, 1})
	okROP(faiImportOperation(request))
	okROP([]byte{ropCreateAttachment, 4, 3, 4})
	replaceIDs := propertyResponseIDs(t, ROPBuffer{Operations: okROP(propertyIDsOperation(4, 1, 2, exceptionReplaceProperty.name))})
	replaceTag := uint32(replaceIDs[0])<<16 | 0x0040
	okROP([]byte{ropOpenEmbeddedMessage, 4, 4, 5, 0xff, 0x0f, 2})
	exception.Location = "Edited from cache"
	// The second occurrence is in daylight time in Europe/Rome.
	anchorUTC := occurrence.Add(-2 * time.Hour)
	attachment := faiSetOperation(4, faiSetRequest{
		{0x37050003, core.Integer(5)}, {0x7ffd0003, core.Integer(2)}, {0x7ffe000b, core.Bool(true)},
		// An inconsistent outer lookup hint must not select a different occurrence.
		{exceptionReplaceTag, core.Timestamp(anchorUTC.AddDate(0, 0, 7))},
	})
	child := faiSetOperation(5, faiSetRequest{
		{0x001a001f, core.Text(calendarExceptionClass)}, {0x0037001f, core.Text(exception.Subject)},
		{replaceTag, core.Timestamp(anchorUTC)}, {locationTag, core.Text(exception.Location)},
		{appointmentStartDateTag, core.Timestamp(exception.WallStart.Add(-2 * time.Hour))},
		{appointmentEndDateTag, core.Timestamp(exception.WallEnd.Add(-2 * time.Hour))},
	})
	for slot, properties := range map[byte][]byte{4: attachment, 5: child} {
		if reply := okROP(properties); !bytes.Equal(reply, propertyProblemsResponse(0x0a, slot, nil)) {
			t.Fatalf("calendar attachment properties rejected for slot %d: %x", slot, reply)
		}
	}
	if reply := run([]byte{0x0c, 4, 6, 5, 2}, 65535); len(reply) != 15 || binary.LittleEndian.Uint32(reply[2:]) != 0 {
		t.Fatalf("saving the calendar exception message failed: %x", reply)
	}
	// Outlook saves the embedded message, releases the handles and then saves
	// the master without issuing RopSaveChangesAttachment.
	okROP([]byte{1, 4, 5, 1, 4, 4})
	if read(ctx).Identity.Revision != after.Identity.Revision {
		t.Fatal("saving the embedded message wrote to Zimbra")
	}
	after.SeriesSchedule.Exceptions = []core.AppointmentSeriesException{exception}
	okROP(faiSetOperation(3, faiSetRequest{
		{recurTag, encodeRecurrence(after)}, {0x0e1b000b, core.Bool(true)}, {0x0e070003, core.Integer(16)},
	}))
	if reply := run(save, 65535); !bytes.Equal(reply, saved) {
		t.Fatalf("cached exception edit did not save the same master MID: %x", reply[:min(7, len(reply))])
	}
	cached := read(ctx)
	if cached.UID != first.UID || cached.Identity.ID != after.Identity.ID ||
		cached.SeriesSchedule == nil || !sameAppointmentExceptions(cached.SeriesSchedule.Exceptions, []core.AppointmentSeriesException{exception}) {
		t.Fatal("cached exception did not persist with its original anchor")
	}
	for _, slot := range []byte{3, 1} {
		okROP([]byte{1, 4, slot})
	}
}
