// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestModifyRulesPreservesExecuteBatch(t *testing.T) {
	f := newOSTFixture(t)
	request := []byte{ropModifyRules, 4, 1, 0, 1, 0, 1, 3, 0}
	request = append32(request, 0x6681001e) // RuleProvider.
	request = append(request, "Fixture\x00"...)
	request = append32(request, 0x667900fd) // RuleCondition.
	request = append(request, 0, 2, 0, 8)
	request = append32(request, 0x0037001f) // AND(Exists(Subject), Not(Exists(Sender))).
	request = append(request, 2, 8)
	request = append32(request, 0x0c1f001f)
	request = append32(request, 0x668000fe) // RuleActions.
	request = append(request, 1, 0, 9, 0, 0x0b)
	request = append(request, make([]byte, 8)...) // MarkAsRead.

	want := append(ropError(ropModifyRules, 1, NotSupported), ropError(ropTellVersion, 0, 0)...)
	suffix := []byte{ropTellVersion, 4, 0, 1, 0, 0, 0, 0, 0}
	body, _ := f.execute(append(bytes.Clone(request), suffix...), 65535)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, want) {
		t.Fatalf("unexpected response %x", got)
	}

	for end := 3; end < len(request); end++ {
		r := reader{data: request[3:end]}
		if err := skipModifyRules(&r); !errors.Is(err, ErrMalformed) {
			t.Fatalf("truncated rule at byte %d: %v", end, err)
		}
	}
	bad := bytes.Clone(request)
	binary.LittleEndian.PutUint16(bad[len(bad)-11:], 8)
	body, _ = f.execute(append(bad, suffix...), 65535)
	if binary.LittleEndian.Uint32(body[4:]) == 0 {
		t.Fatal("undersized action accepted")
	}
}
