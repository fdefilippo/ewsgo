// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/mail"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

type syncAppendMailbox struct {
	*syncMoveMailbox
	appends           int
	loseReply, loseID bool
}

func (b *syncAppendMailbox) AppendMailMessage(_ context.Context, folder core.ID, raw []byte, options core.MailAppendOptions) (core.Object, error) {
	b.appends++
	obj := messageTestObject("native-import-fixture", folder)
	obj.MIME = bytes.Clone(raw)
	obj.Properties[core.IsDraft], obj.Properties[core.IsRead] = core.Bool(false), core.Bool(options.Read)
	obj.Properties[core.IsFlagged] = core.Bool(options.Flagged)
	b.objects = append(b.objects, obj)
	if b.loseReply {
		if b.loseID {
			return core.Object{}, core.ErrOutcomeUnknown
		}
		return core.Object{ID: obj.ID}, core.ErrOutcomeUnknown
	}
	return obj, nil
}

func (b *syncAppendMailbox) ReadMailMessageMIME(ctx context.Context, folder, id core.ID, deleted bool) (core.Object, error) {
	obj, err := b.Get(ctx, id)
	if err != nil || obj.Parent != folder || deleted {
		return core.Object{}, core.ErrNotFound
	}
	return obj, nil
}

func messageImportFixture() []byte {
	stream := &fastTransfer{limit: maxMessageImportBytes}
	stream.scalar(0x001a001f, core.Text("IPM.Note"))
	stream.scalar(0x007d001f, core.Text("From: sender@example.test\r\nTo: reader@example.test\r\nMessage-ID: <import@example.test>\r\n"))
	stream.scalar(0x0037001f, core.Text("Synthetic import"))
	stream.scalar(0x0e070003, core.Integer(1|16))
	stream.scalar(0x1000001f, core.Text("Synthetic body.\r\n"))
	stream.scalar(0x10130102, core.Binary("<p>Corpo HTML di prova.</p>"))
	stream.scalar(0x0e1b000b, core.Bool(true))
	stream.scalar(0x40160003, core.Integer(0x0e13000d))
	stream.word(0x40000003)
	stream.scalar(attachNumberTag, core.Integer(0))
	stream.scalar(0x37050003, core.Integer(1))
	stream.scalar(0x3707001f, core.Text("prova-è.bin"))
	stream.scalar(0x370e001f, core.Text("application/octet-stream"))
	stream.scalar(attachDataBinary, core.Binary{0, 1, 2, 254, 255})
	stream.word(0x400e0003)
	return stream.data
}

func TestImportedMailMIMEPreservesAlternativeAndFileAcrossRetries(t *testing.T) {
	payload := messageImportFixture()
	first, options, err := importedMailMIME(payload, nil, nil)
	if err != nil || !options.Read {
		t.Fatal("import failed", err)
	}
	second, _, err := importedMailMIME(payload, nil, nil)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("same upload produced different MIME", err)
	}
	projected, err := projectMailContents(core.Object{Kind: core.Message, MIME: first})
	if err != nil || len(projected.attachments) != 1 {
		t.Fatal("cannot read round-trip MIME", err)
	}
	if string(projected.values[fixedContentsProperty(0x10130102)].(core.Binary)) != "<p>Corpo HTML di prova.</p>" ||
		projected.values[fixedContentsProperty(0x1000001f)] != core.Text("Synthetic body.\r\n") {
		t.Fatal("message lost a body alternative")
	}
	file := projected.attachments[0]
	if file[fixedContentsProperty(0x3707001f)] != core.Text("prova-è.bin") ||
		!bytes.Equal(file[fixedContentsProperty(attachDataBinary)].(core.Binary), []byte{0, 1, 2, 254, 255}) {
		t.Fatal("message lost its attachment data or filename")
	}
	marker := bytes.Index(payload, binary.LittleEndian.AppendUint32(nil, 0x40000003))
	if marker < 0 {
		t.Fatal("missing fixture attachment")
	}
	duplicate := append(bytes.Clone(payload), payload[marker:]...)
	for _, malformed := range [][]byte{payload[:len(payload)-1], duplicate} {
		if _, _, err := importedMailMIME(malformed, nil, nil); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("malformed attachments passed validation", err)
		}
	}
}

func TestSQLiteNormalMessageImportResumesWithoutSecondAppend(t *testing.T) {
	for _, loseID := range []bool{false, true} {
		name := "KnownID"
		if loseID {
			name = "MissingID"
		}
		t.Run(name, func(t *testing.T) {
			f, base := newContentsDownloadFixture(t)
			backend := &syncAppendMailbox{
				syncMoveMailbox: &syncMoveMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}},
				loseReply:       true, loseID: loseID,
			}
			f.session = &testSession{backend}
			f.transport.EnableFAICollectorSpike()
			reopen := sqliteExecuteFixture(t, f)
			db := reopen()
			store := replicaStore(t, f)
			reservation, code := store.reserveLocalIDs(t.Context(), 1)
			if code != 0 {
				t.Fatal("missing reserved MID")
			}
			r := syntheticFAIImport()
			r.flags = 0
			copy(r.source[:16], store.replica[:])
			copy(r.source[16:], reservation[:])
			counter := ownMoveCounter(store.replica, r.source)
			mid := folderID(counter)
			inbox := f.fids[Inbox]
			open := func() {
				t.Helper()
				f.handles = append(f.handles, 0xffffffff, 0xffffffff)
				faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
			}
			upload := func(data []byte) {
				t.Helper()
				faiRunOK(t, f, faiImportOperation(r))
				faiRunOK(t, f, []byte{ropFastTransferDestinationConfigure, 4, 3, 4, 1, 0})
				faiRunOK(t, f, faiUploadBuffer(4, data[:3]))
				faiRunOK(t, f, faiUploadBuffer(4, data[3:]))
				faiRunOK(t, f, []byte{1, 4, 4})
			}
			save := []byte{0x0c, 4, 4, 3, 0}
			requireFailure := func(op []byte, output byte, code uint32) {
				t.Helper()
				got, _ := f.run(op)
				if !bytes.Equal(got, ropError(op[0], output, code)) {
					t.Fatalf("unexpected response %x for operation 0x%02x", got, op[0])
				}
			}
			open()
			payload := messageImportFixture()
			upload(payload[:len(payload)-1])
			requireFailure(save, 4, faiError(core.ErrInvalid, true))
			faiRunOK(t, f, []byte{1, 4, 3})
			upload(payload)
			if backend.appends != 0 {
				t.Fatal("upload committed before Save")
			}
			requireFailure(save, 4, faiError(core.ErrOutcomeUnknown, true))
			if backend.appends != 1 || store.byMID[mid] != (messageKey{}) {
				t.Fatal("first attempt published an unconfirmed projection")
			}
			if _, err := store.observeMailMessages(t.Context(), f.session, inbox); !errors.Is(err, core.ErrOutcomeUnknown) {
				t.Fatal("pending native message was presented under another MID")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopen()
			store = replicaStore(t, f)
			open()
			upload(payload)
			if loseID {
				requireFailure(save, 4, faiError(core.ErrOutcomeUnknown, true))
				if backend.appends != 1 {
					t.Fatal("unknown native outcome retried append")
				}
				return
			}
			saved := faiRunOK(t, f, save)
			if len(saved) != 15 || binary.LittleEndian.Uint64(saved[7:]) != mid || backend.appends != 1 {
				t.Fatal("retry did not publish the reserved MID without reappending")
			}
			requireFailure(faiImportOperation(r), 3, faiError(errFAIIgnore, false))
			snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
			if err != nil || len(snapshot.messages) != 2 {
				t.Fatal("destination did not expose one new message", err)
			}
			version := store.messages[store.byMID[mid]]
			if !version.read || version.change != r.change {
				t.Fatal("import lost its wire metadata")
			}
			faiRunOK(t, f, []byte{0x82, 4, 2, 5})
			state := collectorThreeTestSets(t, drainFX(t, f.run, 5, 300), idSet{})
			if !state.seen.contains(store.replica, version.cn) {
				t.Fatal("collector did not acknowledge the imported message")
			}
		})
	}
}

func TestNormalMessageImportStagesPropertiesAndAttachmentBeforeSave(t *testing.T) {
	t.Run("WriteStream", func(t *testing.T) { testNormalMessageImportAttachment(t, false) })
	t.Run("CopyToStream", func(t *testing.T) { testNormalMessageImportAttachment(t, true) })
}

type streamCopyImportMailbox struct{ *syncAppendMailbox }

func (b *streamCopyImportMailbox) ReadMailMessageEnvelope(ctx context.Context, folder, id core.ID, softDeleted bool) (core.Object, error) {
	object, err := b.ReadMailMessageMIME(ctx, folder, id, softDeleted)
	object.MIME = nil
	return object, err
}

func testNormalMessageImportAttachment(t *testing.T, copyStream bool) {
	f, base := newContentsDownloadFixture(t)
	box := &syncAppendMailbox{syncMoveMailbox: &syncMoveMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}}}
	f.session = &testSession{&streamCopyImportMailbox{box}}
	f.transport.EnableFAICollectorSpike()
	sqliteExecuteFixture(t, f)()
	store := replicaStore(t, f)
	reservation, code := store.reserveLocalIDs(t.Context(), 1)
	if code != 0 {
		t.Fatal("missing reserved MID")
	}
	request := syntheticFAIImport()
	request.flags = 0
	copy(request.source[:16], store.replica[:])
	copy(request.source[16:], reservation[:])
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff)
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})

	metadata := faiSetRequest{
		{0x001a001f, core.Text("IPM.Note")},
		{0x0037001f, core.Text("Synthetic direct import")},
		{0x0e070003, core.Integer(1 | 16)},
		{0x007d001f, core.Text("From: sender@example.test\r\nTo: reader@example.test\r\nMessage-ID: <import@example.test>\r\n")},
		{0x12340003, core.Integer(123)}, // Individually refused, with the other properties retained.
		{flagStatusTag, core.Integer(2)},
		{followupIconTag, core.Integer(6)},
	}
	batch := append(faiImportOperation(request), faiSetOperation(3, metadata)...)
	got, _ := f.run(batch)
	want := binary.LittleEndian.AppendUint64(ropError(0x72, 3, 0), 0)
	want = append(want, propertyProblemsResponse(0x0a, 3, []faiPropertyProblem{{index: 4, tag: metadata[4].tag, code: NotSupported}})...)
	if !bytes.Equal(got, want) || box.appends != 0 {
		t.Fatalf("properties did not stage atomically: %x", got)
	}
	recipient := core.Recipient{Kind: core.RecipientTo, Address: "staged@example.test"}
	wire, ok := encodeOpenRecipient(recipient)
	if !ok {
		t.Fatal("cannot encode synthetic recipient")
	}
	modify := append32([]byte{0x0e, 4, 3, 0, 0, 1, 0}, 7)
	modify = append(modify, wire[0])
	modify = append(modify, wire[5:]...) // Row size and RecipientRow.
	faiRunOK(t, f, modify)
	faiRunOK(t, f, []byte{0x0d, 4, 3, 0, 0, 0, 0})
	faiRunOK(t, f, modify)
	save := []byte{0x0c, 4, 4, 3, 0}
	props := folderPropertiesOperation(4, 3, 1, 0x674a0014, 0x65e20102, 0x1000001f)
	all := allPropertiesWire(4, 3, true)
	list := []byte{0x09, 4, 3}
	for _, read := range [][]byte{props, all, list} {
		if got, _ := f.run(read); !bytes.Equal(got, ropError(read[0], 3, NotSupported)) {
			t.Fatalf("ROP %#x read an unpublished import", read[0])
		}
	}
	if got, _ := f.run(save); len(got) != 6 || binary.LittleEndian.Uint32(got[2:]) == 0 || box.appends != 0 {
		t.Fatal("save without a body had a side effect")
	}
	faiRunOK(t, f, faiSetOperation(3, faiSetRequest{
		{0x1000001f, core.Text("Body received through SetProperties.")},
		{0x10130102, core.Binary("<p>HTML received through SetProperties.</p>")},
	}))
	const attachment, stream = 5, 6
	create := append([]byte{ropCreateAttachment, 4, 3, attachment}, faiSetOperation(attachment, faiSetRequest{
		{0x37050003, core.Integer(1)},
		{0x3707001f, core.Text("prova-è.bin")},
		{0x370e001f, core.Text("application/octet-stream")},
	})...)
	wantCreate := append32(ropError(ropCreateAttachment, attachment, 0), 0)
	wantCreate = append(wantCreate, propertyProblemsResponse(0x0a, attachment, nil)...)
	if got := faiRunOK(t, f, create); !bytes.Equal(got, wantCreate) {
		t.Fatalf("CreateAttachment and SetProperties did not stage the file: %x", got)
	}
	file := bytes.Repeat([]byte{0xff, 0x00, 0xa5, 0x5a}, 24*1024)
	faiRunOK(t, f, streamOpenWire(attachment, stream, 2, attachDataBinary))
	faiRunOK(t, f, binary.LittleEndian.AppendUint64([]byte{ropSetStreamSize, 4, stream}, uint64(len(file))))
	saveAttachment := []byte{ropSaveAttachment, 4, 3, attachment, 2}
	for _, request := range [][]byte{saveAttachment, save} {
		got, _ := f.run(request)
		if len(got) != 6 || binary.LittleEndian.Uint32(got[2:]) == 0 || box.appends != 0 {
			t.Fatalf("ROP %#x accepted incomplete attachment data: %x", request[0], got)
		}
	}
	if copyStream {
		copyImportAttachmentStream(t, f, store, stream, file, &box.objects[0])
	} else {
		for part := range slices.Chunk(file, 16*1024) {
			faiRunOK(t, f, append(binary.LittleEndian.AppendUint16([]byte{ropWriteStream, 4, stream}, uint16(len(part))), part...))
		}
	}
	faiRunOK(t, f, saveAttachment)
	faiRunOK(t, f, []byte{1, 4, stream, 1, 4, attachment})
	if box.appends != 0 {
		t.Fatal("SaveChangesAttachment wrote to the mailbox")
	}
	mid := folderID(ownMoveCounter(store.replica, request.source))
	wantSave := binary.LittleEndian.AppendUint64(append(ropError(0x0c, 4, 0), 3), mid)
	wantProps := binary.LittleEndian.AppendUint64(append(ropError(0x07, 3, 0), 0), mid)
	wantProps = append(binary.LittleEndian.AppendUint16(wantProps, uint16(len(request.change))), request.change...)
	for _, ch := range "Body received through SetProperties." {
		wantProps = binary.LittleEndian.AppendUint16(wantProps, uint16(ch))
	}
	wantProps = binary.LittleEndian.AppendUint16(wantProps, 0)
	wantBatch := append(bytes.Clone(wantSave), wantProps...)
	batch = append(bytes.Clone(save), props...)
	maximum := uint32(10 + 4*len(f.handles) + 15 + 7 + 9 + 27 + 5 - 1)
	if response, _ := f.execute(batch, maximum); storeResponse(t, response).Operations[0] != 0xff || box.appends != 0 {
		t.Fatal("insufficient response space executed the native save")
	}
	if got := faiRunOK(t, f, batch); !bytes.Equal(got, wantBatch) || box.appends != 1 {
		t.Fatalf("save and read on import handle returned %x, want %x", got, wantBatch)
	}
	if got := faiRunOK(t, f, batch); !bytes.Equal(got, wantBatch) || box.appends != 1 {
		t.Fatal("repeating the save and read changed the message or appended a duplicate")
	}
	opened := byte(len(f.handles))
	f.handles = append(f.handles, 0xffffffff)
	faiRunOK(t, f, openMessageOperation(4, 1, opened, f.fids[Inbox], mid, 0x0fff, 0))
	for _, read := range [][]byte{all, list} {
		ordinary := bytes.Clone(read)
		ordinary[2] = opened
		want := faiRunOK(t, f, ordinary)
		want[1] = 3
		if got := faiRunOK(t, f, read); !bytes.Equal(got, want) || len(got) <= 8 || box.appends != 1 {
			t.Fatalf("ROP %#x on the import handle differs from the same opened message", read[0])
		}
		limited, _ := f.execute(read, uint32(10+4*len(f.handles)+8))
		if got := storeResponse(t, limited).Operations; !bytes.Equal(got, ropError(read[0], 3, tableBufferTooSmall)) {
			t.Fatalf("ROP %#x exceeded the enumeration budget: %x", read[0], got)
		}
	}
	message := box.objects[len(box.objects)-1]
	parsed, err := mail.ReadMessage(bytes.NewReader(message.MIME))
	if err != nil || parsed.Header.Get("To") != "<staged@example.test>" || message.Properties[core.IsFlagged] != core.Bool(true) {
		t.Fatal("recipient or active flag was lost when publishing the import", err)
	}
	payload, err := projectMailContents(message)
	if err != nil || payload.values[fixedContentsProperty(0x1000001f)] != core.Text("Body received through SetProperties.") ||
		payload.values[fixedContentsProperty(0x0037001f)] != core.Text("Synthetic direct import") || len(payload.attachments) != 1 {
		t.Fatal("incorrect content saved from direct upload", err)
	}
	gotFile := payload.attachments[0]
	if gotFile[fixedContentsProperty(0x3707001f)] != core.Text("prova-è.bin") ||
		!bytes.Equal(gotFile[fixedContentsProperty(attachDataBinary)].(core.Binary), file) {
		t.Fatal("the attachment did not survive the native import")
	}
}

func copyImportAttachmentStream(t *testing.T, f *ostFixture, store *ramStore, target byte, file []byte, sourceMessage *core.Object) {
	t.Helper()
	const source, message, attachment = 7, 8, 9
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff)
	// Use a one-byte prefix so the copied range starts at a nonzero source offset.
	encoded := base64.StdEncoding.EncodeToString(append([]byte{0x42}, file...))
	sourceMessage.MIME = []byte("Subject: CopyToStream source\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=source.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n" + encoded)
	fid := f.fids[Inbox]
	observed, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(observed.messages) != 1 {
		t.Fatal("copy source fixture unavailable", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, message, fid, observed.messages[0].mid, 0x0fff, 0))
	faiRunOK(t, f, attachmentOpenWire(4, message, attachment, 0, 0))
	faiRunOK(t, f, streamOpenWire(attachment, source, 0, attachDataBinary))
	faiRunOK(t, f, streamSeekWire(source, 0, 1))
	wire := func(from, to byte, count uint64) []byte {
		return binary.LittleEndian.AppendUint64([]byte{0x3a, 4, from, to}, count)
	}
	check := func(request []byte, code uint32, n uint64) {
		t.Helper()
		want := ropError(0x3a, request[2], code)
		if code == 0x503 {
			want = append32(want, uint32(request[3]))
		}
		want = binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(want, n), n)
		if out, counts := f.run(request); !bytes.Equal(out, want) || (counts.errors != 0) != (code != 0) {
			t.Fatalf("CopyToStream returned %x, want %x", out, want)
		}
	}
	check(wire(target, source, 10), accessDenied, 0)
	check(wire(source, message, 10), NotSupported, 0)
	check(wire(source, 10, 10), 0x503, 0)
	check(wire(target, target, 10), 0x8003001e, 0)
	request := wire(source, target, 13)
	maximum := uint32(10 + 4*len(f.handles) + 25)
	if body, _ := f.execute(request, maximum); storeResponse(t, body).Operations[0] != 0xff {
		t.Fatal("CopyToStream executed without enough room for its reply")
	}
	for i := 1; i < len(request); i++ {
		if buf, _ := f.execute(request[:i], 65535); !bytes.Equal(buf, executeError(0x80070057, 65535)) {
			t.Fatalf("truncated CopyToStream accepted at %d bytes", i)
		}
	}
	check(request, 0, 13)
	check(wire(source, target, ^uint64(0)), 0, uint64(len(file)-13))
	check(wire(source, target, 1), 0, 0)
	faiRunOK(t, f, []byte{1, 4, source, 1, 4, attachment, 1, 4, message})
}
