// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"strconv"
)

// Contains only bounded counts of completed handler results. It cannot retain
// an EntryID, tag, source role, error code, value or request/response payload.
type CalendarReferenceSetDiagnostic struct {
	Requests, Confirmed, PropertyProblems, OperationErrors int
}

func (CalendarReferenceSetDiagnostic) String() string {
	return "CalendarReferenceSetDiagnostic{redacted}"
}
func (CalendarReferenceSetDiagnostic) GoString() string {
	return "CalendarReferenceSetDiagnostic{redacted}"
}

func (d *CalendarReferenceSetDiagnostic) record(response []byte) {
	if d.Requests >= 1024 {
		return
	}
	d.Requests++
	if len(response) < 8 || response[0] != 0xa || binary.LittleEndian.Uint32(response[2:]) != 0 {
		d.OperationErrors++
		return
	}
	if binary.LittleEndian.Uint16(response[6:]) != 0 {
		d.PropertyProblems++
		return
	}
	d.Confirmed++
}

func (d CalendarReferenceSetDiagnostic) Fields() ([]string, string) {
	if d.Requests < 0 || d.Requests > 1024 || d.Confirmed < 0 || d.PropertyProblems < 0 || d.OperationErrors < 0 || d.Confirmed > d.Requests || d.PropertyProblems > d.Requests-d.Confirmed || d.OperationErrors != d.Requests-d.Confirmed-d.PropertyProblems {
		return []string{"other"}, "other"
	}
	out := []string{}
	if d.Confirmed > 0 {
		out = append(out, "confirmed")
	}
	if d.PropertyProblems > 0 {
		out = append(out, "property_problem")
	}
	if d.OperationErrors > 0 {
		out = append(out, "operation_error")
	}
	if len(out) == 0 {
		out = []string{"none"}
	}
	return out, strconv.Itoa(d.Requests)
}

func (t *SessionTransport) EnableCalendarReferenceSetTrace() { t.calendarReferenceSetTrace = true }

// For a mixed reference Set, classify only Calendar's own result. Other role
// problems cannot become a Calendar failure or enter this fixed diagnostic.
func (d *CalendarReferenceSetDiagnostic) recordReferenceSet(response []byte, set *specialFolderReferenceSet) {
	for index, value := range set.values {
		if value.tag != calendarReferenceTag {
			continue
		}
		if len(response) < 8 || binary.LittleEndian.Uint32(response[2:]) != 0 {
			d.record(response)
			return
		}
		n := int(binary.LittleEndian.Uint16(response[6:]))
		if len(response) != 8+10*n {
			d.record(nil)
			return
		}
		calendarProblem := false
		for i := 0; i < n; i++ {
			p := response[8+i*10:]
			calendarProblem = calendarProblem || int(binary.LittleEndian.Uint16(p)) == index
		}
		filtered := []byte{0xa, response[1], 0, 0, 0, 0, 0, 0}
		if calendarProblem {
			filtered[6] = 1
		}
		d.record(filtered)
		return
	}
}
