// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"slices"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func qualifyNativeAppointmentContents(t *testing.T, ctx context.Context, native core.Session, inbox uint64, success func([]byte) []byte) map[string]bool {
	t.Helper()
	box := native.Mailbox()
	folder, err := box.(core.SpecialFolderReader).SpecialFolder(ctx, core.CalendarFolder)
	if err != nil {
		t.Fatal("native Calendar unavailable")
	}
	writer := box.(core.AppointmentWriter)
	reader := box.(core.AppointmentReader)
	patcher := box.(core.AppointmentDetailsWriter)
	prefix := "EWSGo Calendar MAPI " + time.Now().UTC().Format("20060102T150405.000000000 ")
	start := time.Date(2027, time.March, 27, 0, 0, 0, 0, time.UTC)
	checks := map[string]bool{}
	var fixtures []core.Appointment
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		for _, item := range fixtures {
			current, err := reader.GetAppointment(cleanup, folder.ID, item.Identity.ID)
			if err != nil || writer.TrashAppointment(cleanup, current) != nil {
				t.Error("could not trash a synthetic appointment")
			}
		}
		if !t.Failed() {
			checks["syntheticAppointmentsTrashed"] = true
		}
	}()
	for _, item := range []core.Appointment{
		{Subject: prefix + "timed", Body: "Native appointment note", Location: "Room A", Start: start.Add(9 * time.Hour), End: start.Add(10 * time.Hour)},
		{Subject: prefix + "civil", Start: start, End: start.AddDate(0, 0, 2), AllDay: true},
		{Subject: prefix + "master", Start: start.Add(8 * time.Hour), End: start.Add(9 * time.Hour),
			Type: core.AppointmentMaster, TimeZone: "Europe/Rome", Recurrence: &core.AppointmentRecurrence{
				Frequency: core.RecurrenceWeekly, Interval: 1, StartDate: start, Count: 3,
				FirstDay: time.Monday, Days: []time.Weekday{time.Saturday},
			}},
	} {
		saved, err := writer.CreateAppointment(ctx, folder.ID, item)
		if err != nil {
			t.Fatal("could not seed a synthetic appointment")
		}
		fixtures = append(fixtures, saved)
	}

	success(folderOperation(inbox, 4, 0, 1))
	entry := success(folderPropertiesOperation(4, 1, 1, 0x36d00102))
	if len(entry) != 55 || binary.LittleEndian.Uint16(entry[7:9]) != 46 {
		t.Fatal("missing default Calendar EntryID")
	}
	fid := binary.LittleEndian.Uint64(success(conversionOperation(0x44, entry[31:]))[6:])
	success(folderOperation(fid, 4, 0, 1))
	commit, removed := readNativeFolderChangeIndicators(t, success)
	if next, count := readNativeFolderChangeIndicators(t, success); commit == 0 || next != commit || count != removed {
		t.Fatal("unchanged Calendar lost its stable change indicators")
	}

	fields := []calendarNamedProperty{
		calendarSearchNamed(appointmentStartWhole, 0x0040), calendarSearchNamed(appointmentEndWhole, 0x0040),
		calendarSearchNamed(appointmentAllDay, 0x000b), calendarSearchNamed(core.AppointmentRecurring, 0x000b),
		calendarSearchNamed(appointmentLocation, 0x001f),
		calendarSearchNamed(appointmentStateFlags, 0x0003), calendarSearchNamed(appointmentStartZone, 0x0102),
		calendarSearchNamed(appointmentRecur, 0x0102), calendarSearchNamed(appointmentZoneStruct, 0x0102),
		calendarSearchNamed(appointmentGlobalID, 0x0102), calendarSearchNamed(appointmentCleanID, 0x0102),
	}
	names := make([]propertyName, len(fields))
	for i, field := range fields {
		names[i] = field.property.name
	}
	ids := propertyResponseIDs(t, ROPBuffer{Operations: success(propertyIDsOperation(4, 1, 2, names...))})
	tags := []uint32{0x674a0014, 0x001a001f, 0x0037001f, appointmentStartDateTag, appointmentEndDateTag}
	for i, id := range ids {
		tags = append(tags, uint32(id)<<16|uint32(fields[i].property.kind))
	}

	readTable := func() map[string][]any {
		t.Helper()
		wire := success([]byte{5, 4, 1, 2, 0x10})
		if len(wire) != 10 {
			t.Fatal("unexpected Calendar Contents Table response")
		}
		count := binary.LittleEndian.Uint32(wire[6:])
		success(tableColumnsWire(2, tags...))
		out := map[string][]any{}
		for remaining := count; remaining > 0; {
			rows := decodeTableRows(t, success(tableQueryWire(2, 0, 1, uint16(min(remaining, 20)))), tags)
			if len(rows) == 0 || uint32(len(rows)) > remaining {
				t.Fatal("inconsistent Calendar table count")
			}
			for _, row := range rows {
				if subject, ok := row[2].(string); ok {
					if _, exists := out[subject]; exists {
						for _, item := range fixtures {
							if subject == item.Subject {
								t.Fatal("duplicate synthetic appointment row")
							}
						}
					}
					out[subject] = row
				}
			}
			remaining -= uint32(len(rows))
		}
		success([]byte{1, 4, 2})
		return out
	}
	rows := readTable()
	for _, item := range fixtures {
		row := rows[item.Subject]
		start, end := fileTime(item.Start), fileTime(item.End)
		expect := []any{"IPM.Appointment", item.Subject, start, end, start, end, item.AllDay, item.Type == core.AppointmentMaster}
		if len(row) != len(tags) || !reflect.DeepEqual(row[1:9], expect) || row[10] != uint32(0) {
			t.Fatal("native appointment field differs from the Calendar table")
		}
	}
	civil := rows[fixtures[1].Subject][11]
	civilZone := checkCalendarZone(t, civil, "UTC", 0, 0)
	if !bytes.Equal(civilZone[34:], make([]byte, 32)) {
		t.Fatal("floating civil event has unexpected daylight shifts")
	}
	series := rows[fixtures[2].Subject][11]
	seriesZone := checkCalendarZone(t, series, "Europe/Rome", -60, -60)
	word := func(at int) uint16 { return binary.LittleEndian.Uint16(seriesZone[at:]) }
	if word(36) != 10 || word(38) != 0 || word(40) != 5 || word(42) != 3 ||
		word(52) != 3 || word(54) != 0 || word(56) != 5 || word(58) != 2 {
		t.Fatal("native standard and daylight transitions differ")
	}
	checks["nativeZoneWireRulesAndFloatingCivilZone"] = true
	checks["timedDatesAndCivilDatesAcrossDST"] = true
	checks["seriesProjectsOneMaster"] = true
	master := fixtures[2]
	masterRow := rows[master.Subject]
	scheduledMID := masterRow[0].(uint64)
	global, ok := masterRow[14].([]byte)
	if !ok || len(global) != 52+len(master.UID) || master.UID == "" ||
		!bytes.Equal(global[:16], []byte{4, 0, 0, 0, 0x82, 0, 0xe0, 0, 0x74, 0xc5, 0xb7, 0x10, 0x1a, 0x82, 0xe0, 8}) ||
		!bytes.Equal(global[16:36], make([]byte, 20)) ||
		binary.LittleEndian.Uint32(global[36:40]) != uint32(len(master.UID)+12) ||
		string(global[40:52]) != "vCal-Uid\x01\x00\x00\x00" || string(global[52:]) != master.UID ||
		!reflect.DeepEqual(masterRow[15], global) {
		t.Fatal("native appointment UID was not preserved in the MAPI meeting properties")
	}
	checkCalendarWeeklyRecurrence(t, masterRow[12], start, false)
	zoneStruct, valid := masterRow[13].([]byte)
	if !valid || len(zoneStruct) != 48 || !bytes.Equal(zoneStruct[:12], seriesZone[22:34]) ||
		!bytes.Equal(zoneStruct[14:30], seriesZone[34:50]) || !bytes.Equal(zoneStruct[32:], seriesZone[50:]) {
		t.Fatal("legacy recurrence zone is inconsistent with the display zone")
	}
	if rows[fixtures[0].Subject][12] != uint32(PropertyNotFound) {
		t.Fatal("single appointment unexpectedly acquired a recurrence rule")
	}
	checkAppointmentTags := func(recurrence bool) {
		t.Helper()
		wire := success([]byte{0x09, 4, 3})
		if len(wire) < 8 || len(wire) != 8+4*int(binary.LittleEndian.Uint16(wire[6:8])) {
			t.Fatal("invalid Calendar property tag list")
		}
		var listed []uint32
		for at := 8; at < len(wire); at += 4 {
			listed = append(listed, binary.LittleEndian.Uint32(wire[at:]))
		}
		for _, tag := range []uint32{appointmentStartDateTag, appointmentEndDateTag, tags[5], tags[6], tags[14], tags[15]} {
			if !slices.Contains(listed, tag) {
				t.Fatalf("Calendar property tag %#x is missing", tag)
			}
		}
		if slices.Contains(listed, tags[12]) != recurrence {
			t.Fatal("Calendar property tag list does not match the occurrence type")
		}
	}
	config := fullContentsConfig()
	config.flags = 0x21
	run := func(op []byte) ([]byte, storeResults) { return success(op), storeResults{} }
	sync := func(previous *contentsSyncState) ([]byte, contentsSyncState) {
		t.Helper()
		success(contentsConfigureOperation(config, nil))
		if previous != nil {
			uploadContentsWireTestState(t, run, *previous)
		}
		stream := drainFX(t, run, 2, 16384)
		success([]byte{1, 4, 2})
		return stream, contentsFourTestSets(t, stream)
	}
	initial, state := sync(nil)
	if !containsContactFX(t, initial, 0x0037001f, fixtures[1].Subject) ||
		!containsContactFX(t, initial, appointmentBodyTag, fixtures[0].Body) {
		t.Fatal("initial Calendar transfer omitted a civil appointment or appointment notes")
	}
	initialRecurrence := appointmentTransferField(t, initial, scheduledMID, fields[7].property.name)
	checkCalendarWeeklyRecurrence(t, initialRecurrence, start, false)
	checks["initialCalendarCheckpointCarriesAppointmentsAndSeries"] = true
	skip := start.AddDate(0, 0, 7).Add(7 * time.Hour)
	occurrence, err := reader.GetAppointment(ctx, folder.ID, master.Identity.ID+"@"+core.ID(skip.Format("20060102T150405Z")))
	if err != nil || writer.TrashAppointment(ctx, occurrence) != nil {
		t.Fatal("could not cancel the synthetic occurrence")
	}
	afterCancel, count := readNativeFolderChangeIndicators(t, success)
	if afterCancel <= commit || count != removed {
		t.Fatal("canceling an occurrence did not advance the Calendar commit time alone")
	}
	skippedRow := readTable()[master.Subject]
	checkCalendarWeeklyRecurrence(t, skippedRow[12], start, true)
	if skippedRow[0] != scheduledMID {
		t.Fatal("canceling an occurrence changed the master MID")
	}
	success(openMessageOperation(4, 1, 3, fid, scheduledMID, 0x0fff, 0))
	checkAppointmentTags(true)
	recur := success(folderPropertiesOperation(4, 3, 1, tags[12]))
	if len(recur) < 9 || recur[6] != 0 || int(binary.LittleEndian.Uint16(recur[7:9])) != len(recur)-9 {
		t.Fatal("opened calendar recurrence has invalid binary framing")
	}
	checkCalendarWeeklyRecurrence(t, recur[9:], start, true)
	success([]byte{1, 4, 3})
	checks["weeklyRecurrenceAndNativeCanceledOccurrence"] = true
	timed := fixtures[0]
	mid := rows[timed.Subject][0].(uint64)
	success(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 0))
	checkAppointmentTags(false)
	checks["openedAppointmentTagListReflectsNativeProperties"] = true
	readOpened := func(location string) {
		t.Helper()
		wire := success(folderPropertiesOperation(4, 3, 1, tags[5], tags[6], tags[9], appointmentBodyTag))
		expected := binary.LittleEndian.AppendUint64(make([]byte, 1), fileTime(timed.Start))
		expected = binary.LittleEndian.AppendUint64(expected, fileTime(timed.End))
		expected = append(expected, contentsUnicode(location)...)
		expected = append(expected, contentsUnicode(timed.Body)...)
		if !bytes.Equal(wire[6:], expected) {
			t.Fatal("opened Calendar properties differ from the native appointment")
		}
	}
	readOpened("Room A")
	checks["openedAppointmentTimeLocationAndNotes"] = true
	location := "Room B"
	if _, err := patcher.UpdateAppointmentDetails(ctx, timed, core.AppointmentDetailsPatch{Location: &location}); err != nil {
		t.Fatal("native appointment update failed")
	}
	afterEdit, count := readNativeFolderChangeIndicators(t, success)
	if afterEdit <= afterCancel || count != removed {
		t.Fatal("editing an appointment did not advance the Calendar commit time alone")
	}
	readOpened(location)
	rows = readTable()
	row := rows[timed.Subject]
	if len(row) != len(tags) || row[0] != mid || row[9] != location {
		t.Fatal("native update failed to preserve the MID and refresh the table")
	}
	checks["nativeUpdateVisibleWithStableMID"] = true
	success([]byte{1, 4, 3})
	delta, next := sync(&state)
	if appointmentTransferField(t, delta, mid, fields[4].property.name) != location ||
		containsContactFX(t, delta, 0x0037001f, fixtures[1].Subject) {
		t.Fatal("Calendar delta lost the native update or repeated an unchanged appointment")
	}
	checkCalendarWeeklyRecurrence(t, appointmentTransferField(t, delta, scheduledMID, fields[7].property.name), start, true)
	if !reflect.DeepEqual(appointmentTransferField(t, initial, scheduledMID, fields[9].property.name), global) ||
		!reflect.DeepEqual(appointmentTransferField(t, delta, scheduledMID, fields[9].property.name), global) ||
		!reflect.DeepEqual(appointmentTransferField(t, delta, scheduledMID, fields[10].property.name), global) {
		t.Fatal("Calendar transfer changed the appointment UID after a native edit")
	}
	checks["globalAppointmentUIDPreservedInTableAndCache"] = true
	unchanged, _ := sync(&next)
	for _, token := range decodeTestContents(t, unchanged) {
		if token.tag == 0x40150003 {
			t.Fatal("unchanged Calendar checkpoint repeated an appointment")
		}
	}
	checks["calendarDeltaCarriesNativeEditAndCanceledOccurrenceOnce"] = true

	anchor := start.AddDate(0, 0, 14).Add(7 * time.Hour)
	occurrence, err = reader.GetAppointment(ctx, folder.ID, master.Identity.ID+"@"+core.ID(anchor.Format("20060102T150405Z")))
	if err != nil {
		t.Fatal("could not locate the DST-adjusted occurrence")
	}
	subject, room := "Synthetic exception", "Sala Ω"
	laterStart, laterEnd := anchor.Add(26*time.Hour), anchor.Add(27*time.Hour)
	exception, err := patcher.UpdateAppointmentDetails(ctx, occurrence, core.AppointmentDetailsPatch{
		Subject: &subject, Location: &room, Start: &laterStart, End: &laterEnd,
	})
	if err != nil || exception.Type != core.AppointmentException || !exception.OriginalStart.Equal(anchor) {
		t.Fatal("could not modify the native occurrence")
	}
	updated, _ := sync(&next)
	modified := appointmentTransferField(t, updated, scheduledMID, fields[7].property.name)
	checkCalendarRecurrenceException(t, modified, start, subject, room)
	row = readTable()[master.Subject]
	if row[0] != scheduledMID || !reflect.DeepEqual(row[12], modified) {
		t.Fatal("modified recurrence changed the master MID or diverged between table and delta")
	}
	checks["calendarNativeExceptionPreservesOriginalOccurrenceAndDST"] = true

	current, err := reader.GetAppointment(ctx, folder.ID, timed.Identity.ID)
	if err != nil || writer.TrashAppointment(ctx, current) != nil {
		t.Fatal("could not trash the synthetic appointment")
	}
	fixtures = fixtures[1:]
	if next, count := readNativeFolderChangeIndicators(t, success); next <= afterEdit || count != removed+1 {
		t.Fatal("trashing an appointment did not advance both Calendar indicators")
	}
	checks["calendarChangeIndicatorsTrackEditsCanceledOccurrenceAndRemoval"] = true
	return checks
}

func checkCalendarRecurrenceException(t *testing.T, raw any, start time.Time, subject, room string) {
	t.Helper()
	wire, ok := raw.([]byte)
	reader := reader{data: wire}
	readDword := func() uint32 {
		data := reader.take(4)
		if reader.err != nil {
			t.Fatal("truncated recurrence exception")
		}
		return binary.LittleEndian.Uint32(data)
	}
	readWord := func() uint16 {
		data := reader.take(2)
		if reader.err != nil {
			t.Fatal("truncated recurrence exception")
		}
		return binary.LittleEndian.Uint16(data)
	}
	midnight := uint32(fileTime(start) / (60 * 10_000_000))
	originalDay, movedDay := midnight+14*minutesPerDay, midnight+15*minutesPerDay
	reader.take(38)
	if !ok || readDword() != 2 || readDword() != midnight+7*minutesPerDay || readDword() != originalDay ||
		readDword() != 1 || readDword() != movedDay {
		t.Fatal("recurrence dates omitted the canceled or modified occurrence")
	}
	reader.take(6 * 4)
	checkTimes := func() {
		t.Helper()
		if readDword() != movedDay+11*60 || readDword() != movedDay+12*60 || readDword() != originalDay+9*60 {
			t.Fatal("exception bounds lost the local time or original recurrence anchor")
		}
	}
	if readWord() != 1 {
		t.Fatal("unexpected exception count")
	}
	checkTimes()
	if readWord() != 0x11 {
		t.Fatal("missing exception subject and location overrides")
	}
	for _, text := range []string{subject, "Sala ?"} {
		if readWord() != uint16(len(text)+1) || readWord() != uint16(len(text)) ||
			string(reader.take(len(text))) != text {
			t.Fatal("legacy exception text mismatch")
		}
	}
	if readDword() != 0 || readDword() != 4 || readDword() != 0 || readDword() != 0 {
		t.Fatal("misframed extended exception")
	}
	checkTimes()
	for _, text := range []string{subject, room} {
		unicode := contentsUnicode(text)
		size := len(unicode) - 2
		if readWord() != uint16(size/2) || !bytes.Equal(reader.take(size), unicode[:size]) {
			t.Fatal("Unicode exception text mismatch")
		}
	}
	if readDword() != 0 || readDword() != 0 || reader.pos != len(wire) {
		t.Fatal("misframed recurrence trailer")
	}
}

func appointmentTransferField(t *testing.T, stream []byte, mid uint64, name propertyName) any {
	t.Helper()
	var current uint64
	for _, token := range decodeTestContents(t, stream) {
		switch token.tag {
		case 0x40120003:
			current = 0
		case 0x674a0014:
			current = token.value.(uint64)
		}
		if current == mid && token.set == name.Set && token.lid == name.LID {
			return token.value
		}
	}
	t.Fatal("Calendar FastTransfer omitted a named property for the expected MID")
	return nil
}

func checkCalendarWeeklyRecurrence(t *testing.T, raw any, date time.Time, skipped bool) {
	t.Helper()
	wire, ok := raw.([]byte)
	length := 80
	if skipped {
		length += 4
	}
	if !ok || len(wire) != length ||
		!bytes.Equal(wire[:10], []byte{4, 0x30, 4, 0x30, 0x0b, 0x20, 1, 0, 0, 0}) {
		t.Fatalf("missing or malformed weekly recurrence, wire length %d", len(wire))
	}
	dword := func(at int) uint32 { return binary.LittleEndian.Uint32(wire[at:]) }
	midnight := uint32(fileTime(date) / (60 * 10_000_000))
	if dword(10) != (midnight-5*1440)%10080 || dword(14) != 1 || dword(22) != 0x40 ||
		dword(26) != 0x2022 || dword(30) != 3 || dword(34) != 1 {
		t.Fatal("incorrect weekly cycle, weekday mask or range")
	}
	dates := 42
	if skipped {
		dates += 4
		if dword(38) != 1 || dword(42) != midnight+7*1440 {
			t.Fatal("canceled occurrence missing from the DeletedInstanceDates set")
		}
	} else if dword(38) != 0 {
		t.Fatal("unexpected canceled occurrence")
	}
	if dword(dates) != 0 || dword(dates+4) != midnight || dword(dates+8) != midnight+14*1440 ||
		dword(dates+12) != 0x3006 || dword(dates+16) != 0x3009 ||
		dword(dates+20) != 9*60 || dword(dates+24) != 10*60 {
		t.Fatal("incorrect master or local recurrence times")
	}
}

func checkCalendarZone(t *testing.T, raw any, name string, utcBias, daylightAdjustment int32) []byte {
	t.Helper()
	key := contentsUnicode(name)
	key = key[:len(key)-2]
	wire, ok := raw.([]byte)
	if !ok || len(wire) != 10+len(key)+66 || !bytes.Equal(wire[:2], []byte{2, 1}) ||
		binary.LittleEndian.Uint16(wire[2:]) != uint16(6+len(key)) ||
		binary.LittleEndian.Uint16(wire[4:]) != 2 ||
		binary.LittleEndian.Uint16(wire[6:]) != uint16(len(key)/2) ||
		!bytes.Equal(wire[8:8+len(key)], key) || binary.LittleEndian.Uint16(wire[8+len(key):]) != 1 {
		t.Fatal("missing or malformed appointment display zone")
	}
	rule := wire[10+len(key):]
	if !bytes.Equal(rule[:6], []byte{2, 1, 0x3e, 0, 2, 0}) ||
		int32(binary.LittleEndian.Uint32(rule[22:])) != utcBias ||
		binary.LittleEndian.Uint32(rule[26:]) != 0 ||
		int32(binary.LittleEndian.Uint32(rule[30:])) != daylightAdjustment {
		t.Fatal("invalid effective appointment time zone rule")
	}
	return rule
}
