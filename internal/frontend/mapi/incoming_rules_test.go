// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

type ruleFixtureMailbox struct {
	*messageFixtureMailbox
	snapshot core.InboxRules
	fail     error
	writes   int
}

func (b *ruleFixtureMailbox) ReadInboxRules(context.Context) (core.InboxRules, error) {
	return b.snapshot, nil
}
func (b *ruleFixtureMailbox) ReplaceInboxRules(_ context.Context, expected string, rules []core.InboxRule) error {
	b.writes++
	if expected != b.snapshot.Revision {
		return core.ErrConflict
	}
	if b.fail != nil {
		return b.fail
	}
	b.snapshot.Rules = slices.Clone(rules)
	return nil
}

func TestInboxReportsRulePresence(t *testing.T) {
	f, m := newContentsDownloadFixture(t)
	box := &ruleFixtureMailbox{messageFixtureMailbox: m}
	f.session = &testSession{box}
	check := func(role SpecialFolderRole, present byte) {
		t.Helper()
		out, _ := f.run(folderOperation(f.fids[role], 4, 0, 1))
		if !bytes.Equal(out, append(ropError(2, 1, 0), present, 0)) {
			t.Fatalf("OpenFolder rule presence: %x", out)
		}
		out, _ = f.run(folderPropertiesOperation(4, 1, 1, folderHasRules))
		if !bytes.Equal(out, append(ropError(7, 1, 0), 0, present)) {
			t.Fatalf("PidTagHasRules: %x", out)
		}
	}
	check(Inbox, 0)
	box.snapshot.Rules = []core.InboxRule{{Name: "disabled rule", MarkRead: true, SubjectContains: "lab"}}
	check(Inbox, 1)
	check(SentItems, 0)
	box.snapshot.Rules = nil
	check(Inbox, 0)
}

func TestIncomingRulesWireRoundTripAndAtomicBatch(t *testing.T) {
	t.Run("mark read", func(t *testing.T) {
		checkIncomingRulesWireRoundTrip(t, markReadRuleAction[:])
	})
	t.Run("opaque deferred action", func(t *testing.T) {
		wire := []byte{1, 0, 12, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0x00, 0xfd, 0xff}
		checkIncomingRulesWireRoundTrip(t, wire)
	})
	t.Run("mark read plus deferred", func(t *testing.T) {
		wire := append([]byte{2, 0}, markReadRuleAction[2:]...)
		wire = append(wire, 12, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0x00, 0xfd, 0xff)
		checkIncomingRulesWireRoundTrip(t, wire)
	})
}

func checkIncomingRulesWireRoundTrip(t *testing.T, action []byte) {
	t.Helper()
	f, m := newContentsDownloadFixture(t)
	box := &ruleFixtureMailbox{messageFixtureMailbox: m, snapshot: core.InboxRules{Revision: "fixture revision"}}
	f.session = &testSession{box}
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true

	condition := append32([]byte{3}, 0x10001)
	condition = append32(append32(condition, 0x0037001f), 0x0037001f)
	wstr := func(s string) []byte {
		b := make([]byte, 0, 2*len(s)+2)
		for i := range len(s) {
			b = append(b, s[i], 0)
		}
		return append(b, 0, 0)
	}
	condition = append(condition, wstr("lab match")...)
	provider := wstr("RuleOrganizer2")
	providerData := []byte{3, 0, 0xff, 0, 0xcc}
	add := func(actions []byte) []byte {
		row := append32([]byte{1, 6, 0}, 0x6682001f)
		row = append(row, wstr("lab rule")...)
		row = append(append32(row, 0x6681001f), provider...)
		row = append(append32(row, ruleProviderData), providerData...)
		row = append32(append32(row, 0x66770003), 0x11)
		row = append(append32(row, 0x667900fd), condition...)
		return append(append32(row, 0x668000fe), actions...)
	}
	do := func(code uint32, rows ...[]byte) {
		t.Helper()
		request := binary.LittleEndian.AppendUint16([]byte{0x41, 4, 1, 0}, uint16(len(rows)))
		for _, row := range rows {
			request = append(request, row...)
		}
		out, _ := f.run(request)
		if !bytes.Equal(out, ropError(0x41, 1, code)) {
			t.Fatalf("modify rules response %x", out)
		}
	}
	bad := slices.Clone(action)
	bad[4] = 0x0a
	do(NotSupported, add(action), add(bad))
	if box.writes != 0 {
		t.Fatal("an unsupported second row must prevent the whole write")
	}
	box.fail = core.ErrConflict
	do(0x80040109, add(action))
	if len(box.snapshot.Rules) != 0 {
		t.Fatal("a concurrent edit changed the fixture")
	}
	box.fail = nil
	do(0, add(action))

	out, _ := f.run([]byte{0x3f, 4, 1, 2, 0x40})
	if !bytes.Equal(out, ropError(0x3f, 2, 0)) {
		t.Fatalf("rules table response %x", out)
	}
	out, _ = f.run(tableColumnsWire(2, 0x66740014, 0x667900fd, 0x668000fe, ruleProvider, ruleProviderData))
	if binary.LittleEndian.Uint32(out[2:6]) != 0 {
		t.Fatalf("rules table columns response %x", out)
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 10))
	if len(out) < 18 || binary.LittleEndian.Uint16(out[7:9]) != 1 || out[9] != 0 ||
		!bytes.Equal(out[18:], bytes.Join([][]byte{condition, action, provider, providerData}, nil)) {
		t.Fatalf("rule condition or action returned with incorrect framing: %x", out)
	}
	id := binary.LittleEndian.Uint64(out[10:18])
	remove := binary.LittleEndian.AppendUint64(append32([]byte{4, 1, 0}, 0x66740014), id)
	do(0, remove)
	if len(box.snapshot.Rules) != 0 {
		t.Fatal("remove did not write the empty ruleset")
	}
}
