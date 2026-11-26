// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"reflect"
	"testing"
)

func rejectedBindingFilter(t *testing.T) []byte {
	return filterWire(t, "01 0500 08 0300fe0f 06 01 03003412 08000000 08 1f000180 08 03000000 08 0300ffff")
}
func TestContentsBindingDiagnosticAllArmsAndSharedTagBudget(t *testing.T) {
	var registry namedPropertyRegistry
	c := contentsConfigure{frame: fullContentsConfig()}
	c.frame.flags &^= 0x20
	c.restriction, _ = parseContentsRestriction(rejectedBindingFilter(t))
	var d StorePropertyDiagnostic
	d.unknown.enabled = true
	recordContentsBindingDiagnostic(c, &registry, &d)
	u := d.UnknownProperties()
	if !reflect.DeepEqual(u.Tags, []string{"0x12340003"}) || u.HighIDs != 1 || u.ReservedIDs != 2 || !reflect.DeepEqual(d.PropertyNames(), []string{"object_type"}) {
		t.Fatal("binding diagnostic filters changed", u)
	}
	if d.Requests != 0 || d.Requested != 0 || d.Values != 0 || d.Errors != 0 || d.Other != 0 || d.Folder || d.Logon || len(registry.names) != 0 {
		t.Fatal("diagnostic changed counters or registry")
	}
	// Quota is shared with properties requested both before and after binding.
	var shared StorePropertyDiagnostic
	shared.unknown.enabled = true
	var before, after []uint32
	for i := range 15 {
		before = append(before, uint32(0x4000+i)<<16|3)
		after = append(after, uint32(0x5000+i)<<16|3)
	}
	shared.request(before)
	recordContentsBindingDiagnostic(c, &registry, &shared)
	shared.request(after)
	u = shared.UnknownProperties()
	if len(u.Tags) != 16 || u.Tags[15] != "0x12340003" || u.Overflow != 15 || shared.Requests != 2 || shared.Requested != 30 || shared.Other != 30 {
		t.Fatal("binding/GetProperties quota not shared", u)
	}
	var off StorePropertyDiagnostic
	recordContentsBindingDiagnostic(c, &registry, &off)
	if off != (StorePropertyDiagnostic{}) {
		t.Fatal("default-off diagnostic retained tags")
	}
}

func TestContentsBindingDiagnosticDispatchGateAndFirstFailedConfigure(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, options := range []bool{false, true} {
			f, box := newContentsDownloadFixture(t)
			f.transport.unknownPropertyTrace = unknown
			f.transport.synchronizationOptionsTrace = options
			c := fullContentsConfig()
			c.flags &^= 0x20
			first := contentsConfigureOperation(c, rejectedBindingFilter(t))
			second := contentsConfigureOperation(c, filterWire(t, "08 03007856"))
			ops := append(first, second...)
			raw, counts := f.run(ops)
			u := counts.properties.UnknownProperties()
			if counts.errors != 2 || len(raw) != 12 || binary.LittleEndian.Uint32(raw[2:]) != NotSupported || binary.LittleEndian.Uint32(raw[8:]) != NotSupported || box.sourceCalls != 0 {
				t.Fatal("binding diagnostic changed execution")
			}
			if counts.bindingProperties != (unknown && options) {
				t.Fatal("combined gate escaped")
			}
			if unknown && options {
				if !reflect.DeepEqual(u.Tags, []string{"0x12340003"}) || u.HighIDs != 1 || u.ReservedIDs != 2 {
					t.Fatal("later Configure replaced first or arms skipped", u)
				}
			} else if len(u.Tags) != 0 || u.HighIDs != 0 || u.ReservedIDs != 0 {
				t.Fatal("gate-off captured request tags")
			}
			// No tags before output preflight, or from a malformed entire batch.
			for _, bad := range []struct {
				ops []byte
				max uint32
			}{{ops, 1}, {append(bytes.Clone(ops), 0xff, 4, 0), 65535}} {
				_, d := f.execute(bad.ops, bad.max)
				if d.bindingProperties || len(d.properties.UnknownProperties().Tags) != 0 {
					t.Fatal("preflight acquired binding tags")
				}
			}
		}
	}
}

type contentsBindingRecorder struct {
	*httptest.ResponseRecorder
	calls      int
	diagnostic StorePropertyDiagnostic
}

func (w *contentsBindingRecorder) RecordContentsBindingDiagnostic(d StorePropertyDiagnostic) {
	w.calls++
	w.diagnostic = d
}

func TestContentsBindingDiagnosticTransportCarriesSnapshotWithoutFakeRequests(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	transport.EnableHierarchyDownloadSpike()
	transport.EnableContentsDownloadSpike()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff, 0xffffffff}, 4096)
	r := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(r.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), r.Handles, 4096)
	r = storeResponse(t, body)
	c := fullContentsConfig()
	c.flags &^= 0x20
	op := contentsConfigureOperation(c, rejectedBindingFilter(t))
	op[1] = 0
	wire, _ := (ROPBuffer{Operations: op, Handles: r.Handles}).Marshal()
	request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
	for _, enabled := range []bool{false, true} {
		transport.unknownPropertyTrace = enabled
		transport.synchronizationOptionsTrace = enabled
		h := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		h.Header.Set("Content-Type", "application/mapi-http")
		h.Header.Set("X-RequestId", "synthetic-request:1")
		h.Header.Set("X-RequestType", "Execute")
		for _, cookie := range cookies {
			h.AddCookie(cookie)
		}
		w := &contentsBindingRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, h, session)
		cookies = w.Result().Cookies()
		requireTransportCode(t, w.ResponseRecorder, 0)
		if w.calls != boolInt(enabled) || w.diagnostic.Requests != 0 {
			t.Fatal("binding snapshot fabricated requests or escaped gate")
		}
		if enabled && !reflect.DeepEqual(w.diagnostic.UnknownProperties().Tags, []string{"0x12340003"}) {
			t.Fatal("binding tags did not cross transport")
		}
	}
}

func FuzzContentsBindingDiagnosticSharedBounds(f *testing.F) {
	f.Add([]byte{8, 3, 0, 0x34, 0x12}, byte(15))
	f.Fuzz(func(t *testing.T, raw []byte, prefix byte) {
		if len(raw) > 9000 {
			return
		}
		n, err := parseContentsRestriction(raw)
		if err != nil {
			return
		}
		var d StorePropertyDiagnostic
		d.unknown.enabled = true
		for i := 0; i < int(prefix)%20; i++ {
			d.unknown.record(uint32(0x4000+i)<<16 | 3)
		}
		c := contentsConfigure{frame: fullContentsConfig(), restriction: n}
		c.frame.flags &^= 0x20
		recordContentsBindingDiagnostic(c, &namedPropertyRegistry{}, &d)
		u := d.UnknownProperties()
		if len(u.Tags) > 16 || u.HighIDs > 128 || u.ReservedIDs > 128 || d.Requests != 0 {
			t.Fatal("unbounded binding diagnostic")
		}
		for _, tag := range d.unknown.tags[:d.unknown.count] {
			id := uint16(tag >> 16)
			if id == 0 || id >= 0x8000 {
				t.Fatal("private/high/reserved tag exposed")
			}
		}
	})
}
