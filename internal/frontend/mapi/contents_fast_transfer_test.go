// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

// Independent test decoder: no production reader, property serializer or IDSET
// codec is used to establish lexical boundaries/types of the emitted stream.
type decodedContentsToken struct {
	tag                       uint32
	definitionEnd, start, end int
	set                       [16]byte
	name                      string
	lid                       uint32
	value                     any
}

func (decodedContentsToken) String() string   { return "decodedContentsToken{private}" }
func (decodedContentsToken) GoString() string { return "decodedContentsToken{private}" }
func decodeTestContents(t *testing.T, raw []byte) []decodedContentsToken {
	t.Helper()
	at := 0
	take := func(n int) []byte {
		t.Helper()
		if n < 0 || n > len(raw)-at {
			t.Fatal("truncated Contents stream")
		}
		b := raw[at : at+n]
		at += n
		return b
	}
	u16 := func() uint16 { return binary.LittleEndian.Uint16(take(2)) }
	u32 := func() uint32 { return binary.LittleEndian.Uint32(take(4)) }
	text := func(b []byte) string {
		t.Helper()
		if len(b) < 2 || len(b)%2 != 0 || b[len(b)-1] != 0 || b[len(b)-2] != 0 {
			t.Fatal("invalid FX Unicode termination")
		}
		units := make([]uint16, len(b)/2-1)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		return string(utf16.Decode(units))
	}
	var tokens []decodedContentsToken
	for at < len(raw) {
		token := decodedContentsToken{start: at, tag: u32()}
		marker := false
		switch token.tag {
		case 0x40000003, 0x40010003, 0x40020003, 0x400e0003, 0x40030003, 0x40040003, 0x40090003, 0x400b0003, 0x400c0003, 0x400d0003, 0x40100003,
			0x40120003, 0x40130003, 0x40140003, 0x40150003, 0x402f0003, 0x403a0003, 0x403b0003, 0x4074000b, 0x4075000b:
			marker = true
		}
		if !marker {
			if token.tag>>16 >= 0x8000 {
				copy(token.set[:], take(16))
				kind := take(1)[0]
				switch kind {
				case 0:
					token.lid = u32()
				case 1:
					var name []byte
					for {
						unit := u16()
						name = binary.LittleEndian.AppendUint16(name, unit)
						if unit == 0 {
							break
						}
						if len(name) > 254 {
							t.Fatal("named property definition unbounded")
						}
					}
					token.name = text(name)
				default:
					t.Fatal("invalid named kind")
				}
			}
			token.definitionEnd = at
			kind := uint16(token.tag)
			if token.tag == 0x40170003 {
				kind = 0x102
			}
			switch kind {
			case 2:
				token.value = int16(u16())
			case 3:
				token.value = int32(u32())
			case 0x1003:
				count := u32()
				if count > uint32((len(raw)-at)/4) {
					t.Fatal("invalid FX multiple integer count")
				}
				values := make([]int32, count)
				for i := range values {
					values[i] = int32(u32())
				}
				token.value = values
			case 0x14, 0x40:
				token.value = binary.LittleEndian.Uint64(take(8))
			case 0xb:
				b := u16()
				if b > 1 {
					t.Fatal("invalid FX boolean")
				}
				token.value = b == 1
			case 0x1e, 0x1f, 0x102:
				size := u32()
				if size > uint32(len(raw)-at) {
					t.Fatal("invalid FX variable length")
				}
				b := take(int(size))
				if kind == 0x1f {
					token.value = text(b)
				} else {
					token.value = append([]byte{}, b...)
				}
			default:
				t.Fatal("unexpected FX property type")
			}
		}
		token.end = at
		tokens = append(tokens, token)
	}
	return tokens
}

func fullContentsConfig() synchronizationConfigure {
	return synchronizationConfigure{kind: 1, flags: 0x8039, sendOptions: 0x1d, extraFlags: 15}
}
func syntheticContentsChange(t *testing.T, f *hierarchyFixture, box *messageFixtureMailbox) contentsChange {
	t.Helper()
	box.objects[0].MIME = []byte("From: sender@example.invalid\r\nSubject: Synthetic complete subject\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nComplete plain 😀\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=synthetic.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAw==\r\n--b--\r\n")
	box.objects[0].Recipients = []core.Recipient{{Kind: core.RecipientCc, Address: "recipient@example.invalid", DisplayName: "Synthetic recipient"}}
	row := messageObserve(t, f, Inbox).messages[0]
	change, err := f.store.projectContentsRow(row)
	if err != nil {
		t.Fatal("project message", err)
	}
	return change
}
func contentsSelectionForTest(t *testing.T, c synchronizationConfigure, s *ramStore) contentsSelection {
	t.Helper()
	selection, err := newContentsSelection(c, &s.named)
	if err != nil {
		t.Fatal("property selection", err)
	}
	return selection
}

// MS-OXCFXICS 2.2.4.3.23 and the grammar in 2.2.4.2 require the identifying
// property first, regardless of numeric tag order. Check decoded wire tokens,
// without consulting the encoder's property maps or sorting rules.
func contentsTestSubobjectHeadersValid(tokens []decodedContentsToken) bool {
	var required, end uint32
	count := 0
	for i, token := range tokens {
		switch token.tag {
		case 0x40030003, 0x40000003:
			if required != 0 || i+1 >= len(tokens) {
				return false
			}
			required, end = 0x30000003, 0x40040003
			if token.tag == 0x40000003 {
				required, end = 0x0e210003, 0x400e0003
			}
			if tokens[i+1].tag != required {
				return false
			}
			count = 0
		case 0x40040003, 0x400e0003:
			if token.tag != end || count != 1 {
				return false
			}
			required, end = 0, 0
		default:
			if required != 0 && token.tag>>16 == required>>16 {
				if token.tag != required {
					return false
				}
				count++
			}
		}
	}
	return required == 0
}

func TestContentsFastTransferSubobjectIdentityFixedPosition(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	change.payload.recipients = append(change.payload.recipients, contentsValues{
		fixedContentsProperty(0x30000003): core.Integer(1),
		fixedContentsProperty(0x0c150003): core.Integer(3),
		fixedContentsProperty(0x3003001f): core.Text("second@example.invalid"),
	})
	for _, c := range []synchronizationConfigure{
		fullContentsConfig(),
		{kind: 1, flags: 0xa1, sendOptions: 1, tags: []uint32{0x0e12000d, 0x0e13000d}},
	} {
		transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
		if err != nil {
			t.Fatal("encode subobjects", err)
		}
		tokens := decodeTestContents(t, transfer.stream.data)
		transfer.close()
		recipients, attachments := 0, 0
		for _, token := range tokens {
			if token.tag == 0x40030003 {
				recipients++
			}
			if token.tag == 0x40000003 {
				attachments++
			}
		}
		if recipients != 2 || attachments != 1 || !contentsTestSubobjectHeadersValid(tokens) {
			t.Fatal("recipient Rowid or attachment number not first and unique")
		}
	}
}

func TestContentsFastTransferEmbeddedMail(t *testing.T) {
	f, box := messageFixture(t)
	box.objects[0].MIME = []byte(embeddedMailFixture)
	row := messageObserve(t, f, Inbox).messages[0]
	change, err := f.store.projectContentsRow(row)
	if err != nil {
		t.Fatal("project embedded mail:", err)
	}
	config := fullContentsConfig()
	transfer, err := f.store.contentsFastTransfer(t.Context(), config, contentsSelectionForTest(t, config, f.store), contentsDelta{changes: []contentsChange{change}})
	if err != nil {
		t.Fatal("transfer embedded mail:", err)
	}
	defer transfer.close()
	const startAttachment, endAttachment = 0x40000003, 0x400e0003
	const startEmbed, endEmbed = 0x40010003, 0x40020003
	var boundaries []uint32
	embedded, subject, body, recipient, data := false, false, false, false, false
	for _, token := range decodeTestContents(t, transfer.stream.data) {
		switch token.tag {
		case startAttachment, endAttachment, startEmbed, endEmbed:
			boundaries = append(boundaries, token.tag)
			if token.tag == startEmbed {
				embedded = true
			} else if token.tag == endEmbed {
				embedded = false
			}
		case 0x0037001f:
			subject = subject || embedded && token.value == "Attached subject"
		case 0x1000001f:
			body = body || embedded && token.value == "Attached body"
		case 0x3003001f:
			recipient = recipient || embedded && token.value == "inner@example.invalid"
		case 0x37010102:
			data = data || embedded && bytes.Equal(token.value.([]byte), []byte{1, 2, 3})
		}
	}
	want := []uint32{startAttachment, startEmbed, startAttachment, endAttachment, endEmbed, endAttachment}
	if !reflect.DeepEqual(boundaries, want) || !subject || !body || !recipient || !data {
		t.Fatal("FastTransfer lost an embedded object boundary or its contents")
	}
}

func TestContentsFastTransferRejectsMissingOrMistypedRecipientRowid(t *testing.T) {
	for _, value := range []core.Value{nil, core.Text("wrong type")} {
		f, box := messageFixture(t)
		change := syntheticContentsChange(t, f, box)
		delete(change.payload.recipients[0], fixedContentsProperty(0x30000003))
		if value != nil {
			change.payload.recipients[0][fixedContentsProperty(0x30000003)] = value
		}
		c := fullContentsConfig()
		transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
		if transfer != nil {
			transfer.close()
		}
		if !errors.Is(err, core.ErrInvalid) || transfer != nil || f.stores.contents.used != 0 {
			t.Fatal("invalid recipient exposed a stream or leaked its lease")
		}
	}
}

func TestContentsFastTransferIndependentDecoderAndAtomicChunks(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	c := fullContentsConfig()
	state := contentsSyncState{given: singleReplicaSet(f.store.replica, []idRange{{folderGlobalCount(change.row.mid), folderGlobalCount(change.row.mid)}}), seen: singleReplicaSet(f.store.replica, []idRange{{change.row.cn, change.row.cn}}), read: singleReplicaSet(f.store.replica, []idRange{{change.row.readCN, change.row.readCN}})}
	transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}, deleted: []uint64{folderID(500)}, noLonger: []uint64{folderID(501)}, read: []uint64{folderID(502)}, unread: []uint64{folderID(503)}, final: state})
	if err != nil {
		t.Fatal("encode complete Contents", err)
	}
	defer transfer.close()
	if f.stores.contents.used != len(transfer.stream.data)+16*len(transfer.stream.atoms) {
		t.Fatal("retained stream storage not charged after encoding")
	}
	tokens := decodeTestContents(t, transfer.stream.data)
	if tokens[0].tag != 0x4074000b || tokens[1].tag != 0x102 {
		t.Fatal("missing progress total")
	}
	progress := tokens[1].value.([]byte)
	if len(progress) != 32 || binary.LittleEndian.Uint32(progress[4:]) != 0 || binary.LittleEndian.Uint32(progress[16:]) != 1 || binary.LittleEndian.Uint64(progress[24:]) != uint64(change.payload.size) {
		t.Fatal("progress total layout/count/size incorrect")
	}
	var header []uint32
	inHeader := false
	counts := map[uint32]int{}
	values := map[uint32]any{}
	for _, token := range tokens {
		counts[token.tag]++
		values[token.tag] = token.value
		if token.tag == 0x40120003 {
			inHeader = true
			continue
		}
		if token.tag == 0x40150003 {
			inHeader = false
		}
		if inHeader {
			header = append(header, token.tag)
		}
	}
	wantHeader := []uint32{0x65e00102, 0x30080040, 0x65e20102, 0x65e30102, 0x67aa000b, 0x674a0014, 0x0e080003, 0x67a40014}
	if !reflect.DeepEqual(header, wantHeader) {
		t.Fatal("message header fixed order violated")
	}
	if counts[0x4075000b] != 1 || counts[0x40030003] != 1 || counts[0x40040003] != 1 || counts[0x40000003] != 1 || counts[0x400e0003] != 1 || counts[0x40160003] != 2 || counts[0x403a0003] != 1 || counts[0x403b0003] != 1 || tokens[len(tokens)-1].tag != 0x40140003 {
		t.Fatal("Contents grammar incomplete")
	}
	if values[0x1000001f] != "Complete plain 😀" || values[0x0c150003] != int32(2) || !bytes.Equal(values[0x37010102].([]byte), []byte{0, 1, 2, 3}) || values[0x3707001f] != "synthetic.bin" || values[0x67aa000b] != false || values[0x67a40014] != folderID(change.row.cn) {
		t.Fatal("real properties, boolean width, or CN wire value incorrect")
	}
	for _, tag := range []uint32{0x67e50102, 0x40210102, 0x402d0102, 0x402e0102} {
		raw := values[tag].([]byte)
		if len(raw) != 10 || raw[0] != 1 || raw[1] != 0 || raw[2] != 6 || raw[9] != 0 {
			t.Fatal("message IDSET is not REPLID-based")
		}
	}
	for _, tag := range []uint32{0x40170003, 0x67960102, 0x67da0102, 0x67d20102} {
		if counts[tag] != 1 {
			t.Fatal("four Contents states not present")
		}
	}
	var assembled []byte
	for at := 0; at < len(transfer.stream.data); {
		end := transfer.stream.chunkEnd(at, 37)
		if end <= at {
			t.Fatal("adequate chunk made no progress")
		}
		for _, token := range tokens {
			if end > token.start && end < token.definitionEnd {
				t.Fatal("chunk split property definition")
			}
			if token.definitionEnd != 0 && token.tag&0xffff == 0xb && end > token.definitionEnd && end < token.end {
				t.Fatal("chunk split FX boolean")
			}
		}
		assembled = append(assembled, transfer.stream.data[at:end]...)
		at = end
	}
	if !bytes.Equal(assembled, transfer.stream.data) || transfer.stream.chunkEnd(0, 3) != 0 {
		t.Fatal("chunk reconstruction/too-small behavior")
	}
	transfer.close()
	transfer.close()
	if f.stores.contents.used != 0 {
		t.Fatal("stream lease leaked")
	}
}

func TestContentsFastTransferFiltersEmptyListsAndConditionalHeader(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	for _, tc := range []struct {
		flags                                        uint16
		tags                                         []uint32
		wantSubject, wantRecipients, wantAttachments bool
	}{
		{0x21, []uint32{0x0037001f, 0x0e12000d}, false, false, true},
		{0xa1, []uint32{0x0037001f, 0x0e12000d}, true, true, false},
		{0xa1, []uint32{0x65e00102}, false, false, false},
	} {
		c := synchronizationConfigure{kind: 1, flags: tc.flags, sendOptions: 1, tags: tc.tags}
		transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
		if err != nil {
			t.Fatal("filtered serialization", err)
		}
		tokens := decodeTestContents(t, transfer.stream.data)
		counts := map[uint32]int{}
		for _, token := range tokens {
			counts[token.tag]++
		}
		if (counts[0x0037001f] != 0) != tc.wantSubject || (counts[0x40030003] != 0) != tc.wantRecipients || (counts[0x40000003] != 0) != tc.wantAttachments || counts[0x65e00102] != 1 || counts[0x674a0014] != 0 || counts[0x67a40014] != 0 || counts[0x0e080003] != 0 || counts[0x4074000b] != 0 {
			t.Fatal("selection/header flags not respected")
		}
		transfer.close()
	}
	change.payload.recipients = nil
	change.payload.attachments = nil
	c := synchronizationConfigure{kind: 1, flags: 0x21, sendOptions: 1}
	transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
	if err != nil {
		t.Fatal(err)
	}
	defer transfer.close()
	count := 0
	for _, token := range decodeTestContents(t, transfer.stream.data) {
		if token.tag == 0x40160003 {
			count++
		}
	}
	if count != 2 {
		t.Fatal("selected empty recipient/attachment list missing delete directive")
	}
}

func TestContentsFastTransferFAINamedDefinitionsOrderAndProviderExclusions(t *testing.T) {
	f, box := messageFixture(t)
	normal := syntheticContentsChange(t, f, box)
	record := faiTestRecord(f.store, f.binding.fids[Inbox], 1)
	key := core.Key{Set: "00112233-4455-6677-8899-aabbccddeeff", Name: strings.Repeat("n", 80) + "😀"}
	name, err := faiNamedProperty(key)
	if err != nil {
		t.Fatal(err)
	}
	ids, code := f.store.named.ids(context.Background(), []propertyName{name}, true)
	if code != 0 {
		t.Fatal("register named")
	}
	record.object.Properties[key] = core.Binary{9, 8, 7}
	numericKey := core.Key{Set: key.Set, ID: 0x1234}
	numericName, e := faiNamedProperty(numericKey)
	if e != nil {
		t.Fatal(e)
	}
	numericIDs, numericCode := f.store.named.ids(context.Background(), []propertyName{numericName}, true)
	if numericCode != 0 {
		t.Fatal("register numeric name")
	}
	record.object.Properties[numericKey] = core.Text("Numeric named value 😀")
	record.object.Properties[core.LastModifiedAt] = core.Timestamp(hierarchyTestTime.Add(time.Hour))
	seedFAI(t, f.store, record)
	observation, err := f.store.observeFAIMessages(context.Background(), f.session, f.binding.fids[Inbox])
	if err != nil {
		t.Fatal(err)
	}
	defer observation.close()
	fai, err := f.store.projectContentsRow(observation.snapshot.messages[0])
	if err != nil {
		t.Fatal(err)
	}
	putContentsValue(fai.payload.values, 0x66660003, core.Integer(5)) // Provider internal properties must not escape.
	c := fullContentsConfig()
	c.flags |= 0x4080
	c.tags = []uint32{0x0037001f}
	transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{normal, fai}})
	if err != nil {
		t.Fatal(err)
	}
	defer transfer.close()
	tokens := decodeTestContents(t, transfer.stream.data)
	associated := []bool{}
	named := 0
	numeric := 0
	for _, token := range tokens {
		if token.tag == 0x67aa000b {
			associated = append(associated, token.value.(bool))
		}
		if token.tag == 0x66660003 {
			t.Fatal("provider internal property escaped")
		}
		if token.tag == uint32(ids[0])<<16|0x102 {
			named++
			if token.set != name.Set || token.name != name.Name || !bytes.Equal(token.value.([]byte), []byte{9, 8, 7}) {
				t.Fatal("named definition/value not preserved")
			}
			if transfer.stream.chunkEnd(token.start, 32) != token.start {
				t.Fatal("named propDef not atomic")
			}
		}
		if token.tag == uint32(numericIDs[0])<<16|0x1f {
			numeric++
			if token.set != numericName.Set || token.lid != 0x1234 || token.value != "Numeric named value 😀" {
				t.Fatal("numeric named definition/value not preserved")
			}
		}
	}
	if !reflect.DeepEqual(associated, []bool{true, false}) || named != 1 || numeric != 1 {
		t.Fatal("delivery ordering/fallback, FAI property selection, or name missing")
	}
	progress := tokens[1].value.([]byte)
	if binary.LittleEndian.Uint32(progress[4:]) != 1 || binary.LittleEndian.Uint32(progress[16:]) != 1 {
		t.Fatal("Normal/FAI progress not separated")
	}
}

func TestContentsFastTransferErrorsReleaseBudgetAndPreserveHierarchyQuota(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	c := fullContentsConfig()
	selection := contentsSelectionForTest(t, c, f.store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := f.store.contentsFastTransfer(ctx, c, selection, contentsDelta{changes: []contentsChange{change}}); !errors.Is(err, context.Canceled) || got != nil || f.stores.contents.used != 0 {
		t.Fatal("canceled encoding leaked")
	}
	for _, delta := range []contentsDelta{{changes: []contentsChange{change, change}}, {changes: []contentsChange{{row: messageRow{mid: folderID(1)}}}}, {read: []uint64{2}}, {deleted: []uint64{0}}} {
		if got, err := f.store.contentsFastTransfer(context.Background(), c, selection, delta); err == nil || got != nil || f.stores.contents.used != 0 {
			t.Fatal("invalid encoding exposed partial stream/budget")
		}
	}
	var huge contentsDelta
	for i := 0; i < 5; i++ {
		copy := change
		copy.row.mid = folderID(uint64(i + 1000))
		copy.payload = projectedMessage{values: contentsValues{fixedContentsProperty(0x10130102): core.Binary(bytes.Repeat([]byte{1}, 1<<20))}}
		huge.changes = append(huge.changes, copy)
	}
	if got, err := f.store.contentsFastTransfer(context.Background(), c, selection, huge); err == nil || got != nil || f.stores.contents.used != 0 {
		t.Fatal("encoded overflow exposed partial stream/budget")
	}
	lease, err := f.store.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.contentsFastTransfer(context.Background(), c, selection, contentsDelta{}); err == nil {
		t.Fatal("aggregate quota bypassed")
	}
	lease.release()
	hierarchy := &fastTransfer{}
	hierarchy.append(make([]byte, maxFastTransferBytes+1), false)
	if hierarchy.err == nil {
		t.Fatal("hierarchy quota implicitly increased")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", contentsDelta{changes: []contentsChange{change}}, contentsDelta{changes: []contentsChange{change}}), "Synthetic") {
		t.Fatal("diagnostic exposed private projection")
	}
}

func TestContentsFastTransferFlagContractAndBestBodySelection(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	for _, modify := range []func(*synchronizationConfigure){
		func(c *synchronizationConfigure) { c.kind = 2 }, func(c *synchronizationConfigure) { c.flags &^= 1 }, func(c *synchronizationConfigure) { c.flags |= 0x1000 }, func(c *synchronizationConfigure) { c.flags |= 0x40 }, func(c *synchronizationConfigure) { c.sendOptions = 0 }, func(c *synchronizationConfigure) { c.sendOptions |= 2 }, func(c *synchronizationConfigure) { c.sendOptions |= 0x80 },
	} {
		c := fullContentsConfig()
		modify(&c)
		if _, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}}); err == nil || f.stores.contents.used != 0 {
			t.Fatal("unsupported configuration encoded/allocated")
		}
	}
	delete(change.payload.values, fixedContentsProperty(0x10130102))
	for _, tc := range []struct {
		flags uint16
		tags  []uint32
		plain bool
	}{
		{0x2000, []uint32{0x1000001f}, true},
		{0x2000, []uint32{0x10000000, 0x10130102}, false},
		{0x2080, []uint32{0x10130102}, false},
	} {
		c := fullContentsConfig()
		c.flags |= tc.flags
		c.tags = tc.tags
		transfer, err := f.store.contentsFastTransfer(t.Context(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
		if err != nil {
			t.Fatal(err)
		}
		hasPlain := false
		for _, token := range decodeTestContents(t, transfer.stream.data) {
			hasPlain = hasPlain || token.tag == 0x1000001f
		}
		transfer.close()
		if hasPlain != tc.plain {
			t.Fatalf("text-only body selection with flags %#x and tags %x", tc.flags, tc.tags)
		}
	}
	putContentsValue(change.payload.values, 0x10130102, core.Binary("<p>real HTML</p>"))
	for _, onlyPlain := range []bool{false, true} {
		c := fullContentsConfig()
		c.flags |= 0x2000
		c.sendOptions |= 0x60
		c.extraFlags |= 0x80000000
		if onlyPlain {
			c.flags |= 0x80
			c.tags = []uint32{0x1000001f}
		}
		transfer, err := f.store.contentsFastTransfer(context.Background(), c, contentsSelectionForTest(t, c, f.store), contentsDelta{changes: []contentsChange{change}})
		if err != nil {
			t.Fatal(err)
		}
		counts := map[uint32]int{}
		for _, token := range decodeTestContents(t, transfer.stream.data) {
			counts[token.tag]++
		}
		if (counts[0x1000001f] > 0) != onlyPlain || (counts[0x10130102] > 0) == onlyPlain {
			t.Fatal("best-body selection dropped the explicitly selected available body")
		}
		transfer.close()
	}
}
