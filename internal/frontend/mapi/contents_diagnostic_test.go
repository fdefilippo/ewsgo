// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestContentsRejectionRetainsLabelAndClosedReasonWithoutPrefix(t *testing.T) {
	for _, tc := range []struct{ wire, reason string }{
		{"05", "restriction_compare_properties"}, {"06 02 0300070e 01000000", "restriction_bitmask"}, {"07", "restriction_size"}, {"09", "restriction_subobject"}, {"0a", "restriction_comment"}, {"0b", "restriction_count"}, {"ff", "restriction_operator"},
		{"08 1e003700", "restriction_string8"}, {"08 1f103700", "restriction_value_type"},
		{"03 00000200 1f003700 1f000000 0000", "restriction_fuzzy_nonspacing"},
		{"03 00000400 1f003700 1f000000 0000", "restriction_fuzzy_loose"},
		{"03 03000000 1f003700 1f000000 0000", "restriction_fuzzy_mode"},
		{"03 00000800 1f003700 1f000000 0000", "restriction_fuzzy_bits"},
		{"03 00000100 02010910 02010000 0000", "restriction_binary_fuzzy"},
		{"04 64 0300070e 03000000 00000000", "restriction_comparison"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			handle := f.handles[1]
			// Frame contains a private tail after the unsupported restriction header.
			// It must never become an error message, diagnostic field or retained value.
			restriction := append(filterWire(t, tc.wire), []byte("PRIVATE-filter-value")...)
			config := fullContentsConfig()
			ops := append([]byte{1, 4, 1}, contentsConfigureOperation(config, restriction)...)
			body, counts := f.execute(ops, 65535)
			if binary.LittleEndian.Uint32(body[4:]) != NotSupported || counts.unsupported != "RopSynchronizationConfigure" {
				t.Fatal("parser rejection lost its public ROP label")
			}
			_, _, _, reasons, present, _ := counts.synchronizationOptions.Fields()
			if !reflect.DeepEqual(reasons, []string{tc.reason}) || present != "present" {
				t.Fatal("missing or spurious active-profile reason", reasons)
			}
			if _, code := f.state.mailbox.object(handle, 4); code != 0 || box.sourceCalls != 0 {
				t.Fatal("rejected batch executed prefix/native capture")
			}
			formatted := fmt.Sprintf("%v %+v %#v %v", counts.synchronizationOptions, counts.synchronizationOptions, counts.synchronizationOptions, reasons)
			if strings.Contains(formatted, "PRIVATE") || strings.Contains(formatted, "0x") {
				t.Fatal("diagnostic exposed client data")
			}
		})
	}
}
func TestContentsDiagnosticMalformedOuterFrameAndRestrictionAreNotPromoted(t *testing.T) {
	c := fullContentsConfig()
	wire := contentsConfigureOperation(c, filterWire(t, "08 1e003700"))
	for n := 0; n < len(wire); n++ {
		_, err := (ROPBuffer{Operations: wire[:n], Handles: []uint32{1, 2, 3}}).decodeContentsOperations(true, true, false, false, false, false, true)
		var u unsupportedOperation
		if errors.As(err, &u) && u.synchronizationOptions.parsed {
			t.Fatal("truncated frame promoted options")
		}
	}
	wire = contentsConfigureOperation(c, filterWire(t, "04 04 1f003700 03000000 00000000"))
	_, err := (ROPBuffer{Operations: wire, Handles: []uint32{1, 2, 3}}).decodeContentsOperations(true, true, false, false, false, false, true)
	if !errors.Is(err, ErrMalformed) {
		t.Fatal("type-mismatched restriction promoted to supported/unsupported profile")
	}
}
func TestContentsOptionDiagnosticMatchesActiveProfile(t *testing.T) {
	base := fullContentsConfig()
	check := func(c synchronizationConfigure) {
		t.Helper()
		_, _, _, reasons, _, _ := inspectContentsOptions(c).Fields()
		if (validateContentsConfigure(c) == nil) != reflect.DeepEqual(reasons, []string{"empty"}) {
			t.Fatal("Contents options reported Hierarchy rejection", reasons)
		}
	}
	check(base)
	for i := range 16 {
		c := base
		c.flags = 1 << i
		check(c)
	}
	for i := range 8 {
		c := base
		c.sendOptions = 1 << i
		check(c)
	}
	for i := range 32 {
		c := base
		c.extraFlags = 1 << i
		check(c)
	}
	for _, kind := range []byte{0, 1, 2, 255} {
		c := base
		c.kind = kind
		check(c)
	}
	// A supported restriction is present, without being a rejection itself.
	config := contentsConfigureOperation(base, filterWire(t, "08 1f003700"))
	ops, err := (ROPBuffer{Operations: config, Handles: []uint32{1, 2, 3}}).decodeContentsOperations(true, true, false, false, false, false, true)
	if err != nil || len(ops) != 1 {
		t.Fatal("supported Contents changed")
	}
	_, _, _, reasons, present, _ := inspectContentsOptions(*ops[0].Configure).Fields()
	if !reflect.DeepEqual(reasons, []string{"empty"}) || present != "present" {
		t.Fatal("present restriction incorrectly rejected")
	}
}
func TestUnsupportedFAIParserRetainsPublicLabelWithoutInventingSyncDiagnostic(t *testing.T) {
	r := syntheticFAIImport()
	r.flags = 0x11
	batch := append([]byte{1, 4, 1}, faiImportOperation(r)...)
	_, err := (ROPBuffer{Operations: batch, Handles: []uint32{1, 2, 3, 4}}).decodeFAIOperations(true, true, false, false, false, false, true, true)
	var u unsupportedOperation
	if !errors.As(err, &u) || u.label != "RopSynchronizationImportMessageChange" || u.synchronizationOptions.parsed {
		t.Fatal("non-Contents rejection lost/misattributed label")
	}
}
func TestContentsRejectionClosedErrorFormattingAndBudget(t *testing.T) {
	_, err := parseContentsRestriction(bytes.Repeat([]byte{0xff}, maxContentsRestrictionBytes+1))
	var reason contentsRestrictionRejection
	if !errors.As(err, &reason) || reason.label() != "restriction_budget" {
		t.Fatal("missing quota reason")
	}
	for i := 0; i < 256; i++ {
		r := contentsRestrictionRejection(i)
		text := fmt.Sprintf("%v %+v %#v", r, r, r)
		if strings.Contains(text, fmt.Sprintf("0x%x", i)) || !strings.Contains(text, "redacted") {
			t.Fatal("reason formatting leaked value")
		}
	}
}

func TestContentsRejectedRestrictionTransportDiagnosticExplicitGate(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	transport.EnableHierarchyDownloadSpike()
	transport.EnableContentsDownloadSpike()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff, 0xffffffff}, 4096)
	response := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(response.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), response.Handles, 4096)
	opened := storeResponse(t, body)
	op := contentsConfigureOperation(fullContentsConfig(), filterWire(t, "08 1e003700"))
	op[1] = 0
	wire, _ := (ROPBuffer{Operations: op, Handles: opened.Handles}).Marshal()
	request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
	for _, enabled := range []bool{false, true} {
		transport.synchronizationOptionsTrace = enabled
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic:1")
		r.Header.Set("X-RequestType", "Execute")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := &syncOptionsRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		cookies = w.Result().Cookies()
		if w.calls != boolInt(enabled) {
			t.Fatal("diagnostic escaped explicit gate")
		}
		if enabled {
			_, _, _, reasons, _, _ := w.diagnostic.Fields()
			if !reflect.DeepEqual(reasons, []string{"restriction_string8"}) {
				t.Fatal("wrong transport reason")
			}
		}
	}
}
func FuzzContentsDiagnosticRejection(f *testing.F) {
	f.Add([]byte{5})
	f.Add([]byte{8, 0x1e, 0, 0x37, 0})
	f.Add([]byte{8, 0x1f, 0, 0x37, 0})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 9000 {
			return
		}
		frame := contentsConfigureOperation(fullContentsConfig(), raw)
		ops, err := (ROPBuffer{Operations: frame, Handles: []uint32{1, 2, 3}}).decodeContentsOperations(true, true, false, false, false, false, true)
		if err == nil {
			if len(ops) != 1 {
				t.Fatal("lost supported frame")
			}
			return
		}
		if ops != nil {
			t.Fatal("partial operation escaped")
		}
		var u unsupportedOperation
		if errors.Is(err, ErrUnsupported) && (!errors.As(err, &u) || u.label != "RopSynchronizationConfigure") {
			t.Fatal("lost rejection label")
		}
		if errors.As(err, &u) && u.synchronizationOptions.parsed {
			_, _, _, reasons, _, _ := u.synchronizationOptions.Fields()
			if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "restriction_") {
				t.Fatal("unbounded or unrelated reason")
			}
		}
	})
}
