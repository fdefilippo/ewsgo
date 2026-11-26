// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func streamOpenWire(input, output, mode byte, tag uint32) []byte {
	return append(append32([]byte{0x2b, 4, input, output}, tag), mode)
}

func streamReadWire(slot byte, count uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{0x2c, 4, slot}, count)
}

func streamSeekWire(slot, origin byte, offset int64) []byte {
	return binary.LittleEndian.AppendUint64([]byte{0x2e, 4, slot, origin}, uint64(offset))
}

func TestDraftAttachmentWriteBatchedWithMessageSaveAndTable(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableFAICollectorSpike()
	f.transport.EnableContentsTableSpike()
	f.transport.EnableMailDraftSending(outboxSendFunc(func(core.Object) (core.SendObservation, error) {
		t.Fatal("incomplete attachment reached the sender")
		return core.SendObservation{}, nil
	}))
	folder := f.state.mailbox.objects[f.handles[1]]
	store := folder.binding.store
	lease, err := store.reserveContentsMemory(2 * core.MaxMailContentMIMEBytes)
	if err != nil {
		t.Fatal(err)
	}
	staged := box.objects[0]
	staged.MIME = []byte("Subject: staged draft\r\nContent-Type: text/plain\r\n\r\nDraft")
	staged.Properties[core.IsDraft] = core.Bool(true)
	draft := &mailDraft{
		store: store, fid: folder.fid, mid: folderID(100), folder: staged.Parent, lease: lease, staged: staged,
		rows: map[uint32]core.Recipient{0: {Address: "synthetic@example.invalid"}},
	}
	handle, err := f.state.mailbox.allocator.handle()
	if err != nil {
		lease.release()
		t.Fatal(err)
	}
	f.state.mailbox.objects[handle] = serverObject{
		logon: 4, binding: folder.binding, fid: folder.fid, message: &openedMessage{mailDraft: draft},
	}
	f.handles[2] = handle
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff)
	const message, attachment, stream, table, saveResponse, clone = 2, 3, 4, 5, 6, 7
	faiRunOK(t, f, []byte{ropCreateAttachment, 4, message, attachment})
	draftFile := f.state.mailbox.objects[f.handles[attachment]].attachment.draft
	draftFile.name = "fixture.bin"
	payload := bytes.Repeat([]byte{0xff, 0, 0x85, 0x32, 0x58}, 14336)
	const chunk = 31744
	write := func(id, slot byte, data []byte) []byte {
		return append(binary.LittleEndian.AppendUint16([]byte{id, 4, slot}, uint16(len(data))), data...)
	}
	wantWrite := func(n int) []byte {
		return binary.LittleEndian.AppendUint16(ropError(ropWriteStream, stream, 0), uint16(n))
	}
	ops := streamOpenWire(attachment, stream, 2, attachDataBinary)
	ops = append(ops, binary.LittleEndian.AppendUint64([]byte{ropSetStreamSize, 4, stream}, uint64(len(payload)))...)
	ops = append(ops, write(ropWriteStream, stream, payload[:chunk])...)
	got := faiRunOK(t, f, ops)
	if !bytes.HasSuffix(got, wantWrite(chunk)) {
		t.Fatal("initial WriteStream failed")
	}
	saveAttachment := []byte{ropSaveAttachment, 4, message, attachment, 2}
	submit := []byte{ropSubmitMessage, 4, message, 0}
	for _, request := range [][]byte{saveAttachment, submit} {
		got, _ = f.run(request)
		if len(got) != 6 || binary.LittleEndian.Uint32(got[2:]) == 0 {
			t.Fatalf("ROP %#x accepted incomplete data", request[0])
		}
	}
	if len(draft.attachmentNumbers) != 0 {
		t.Fatal("an incomplete attachment was installed on the draft")
	}
	zeroWrite := write(ropWriteAndCommitStream, stream, nil)
	if got := faiRunOK(t, f, zeroWrite); !bytes.Equal(got, append(ropError(ropWriteAndCommitStream, stream, 0), 0, 0)) {
		t.Fatal("zero-byte WriteAndCommitStream failed")
	}
	faiRunOK(t, f, streamSeekWire(stream, 0, core.MaxMailContentMIMEBytes))
	overLimit := write(ropWriteAndCommitStream, stream, []byte{1})
	if got, _ = f.run(overLimit); !bytes.Equal(got, ropError(ropWriteAndCommitStream, stream, 0x80030070)) ||
		len(draftFile.data) != len(payload) {
		t.Fatal("WriteAndCommitStream exceeded the size limit")
	}
	faiRunOK(t, f, streamSeekWire(stream, 0, chunk))
	truncated := []byte{ropWriteAndCommitStream, 4, stream, 2, 0, 1}
	body, _ := f.execute(append(write(ropWriteAndCommitStream, stream, []byte{1}), truncated...), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || draftFile.data[chunk] != 0 {
		t.Fatal("malformed WriteAndCommitStream executed the batch prefix")
	}

	// The clone starts at the original's write cursor and changes the same
	// staged attachment. The original must still read from its own cursor.
	faiRunOK(t, f, []byte{ropCloneStream, 4, stream, clone})
	ops = write(ropWriteAndCommitStream, clone, payload[chunk:2*chunk])
	ops = append(ops, 0x0c, 4, saveResponse, message, 2)
	ops = append(ops, 0x05, 4, 1, table, 2)
	ops = append(ops, tableColumnsWire(table, 0x0037001f)...)
	ops = append(ops, tableQueryWire(table, 0, 1, 1)...)
	got = faiRunOK(t, f, ops)
	cloneWritten := binary.LittleEndian.AppendUint16(ropError(ropWriteAndCommitStream, clone, 0), chunk)
	if !bytes.HasPrefix(got, append(cloneWritten, ropError(0x0c, saveResponse, 0)...)) ||
		!bytes.Equal(got[23:29], ropError(0x05, table, 0)) {
		t.Fatal("mixed clone write/save/table batch did not execute in order")
	}
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(stream, 8)), stream); !bytes.Equal(got, payload[chunk:chunk+8]) {
		t.Fatal("source cursor moved or clone write was not visible")
	}
	faiRunOK(t, f, []byte{1, 4, clone})
	faiRunOK(t, f, streamSeekWire(stream, 0, 2*chunk))
	// Exercise the stream cursor after another ROP consumed the previous batch.
	got = faiRunOK(t, f, write(ropWriteStream, stream, payload[2*chunk:]))
	if !bytes.Equal(got, wantWrite(len(payload)-2*chunk)) {
		t.Fatal("last WriteStream failed")
	}
	faiRunOK(t, f, saveAttachment)
	projection, err := projectMailContents(draft.staged)
	if err != nil || len(projection.attachments) != 1 {
		t.Fatal("completed attachment not visible in the draft MIME", err)
	}
	gotData, ok := projection.attachments[0][fixedContentsProperty(attachDataBinary)].(core.Binary)
	if !ok || !bytes.Equal(gotData, payload) || draft.incompleteAttachments != 0 {
		t.Fatal("saved MIME does not contain exactly the written file data")
	}

	faiRunOK(t, f, []byte{1, 4, table})
	faiRunOK(t, f, []byte{ropCreateAttachment, 4, message, table})
	faiRunOK(t, f, faiSetOperation(table, faiSetRequest{
		{tag: 0x3707001f, value: core.Text("survivor.bin")},
		{tag: attachDataBinary, value: core.Binary{1, 2, 3}},
	}))
	faiRunOK(t, f, []byte{ropSaveAttachment, 4, message, table, 2})
	survivor := f.state.mailbox.objects[f.handles[table]].attachment.draft
	faiRunOK(t, f, append32([]byte{ropDeleteAttachment, 4, message}, draftFile.number))
	if survivor.part != 1 || len(draft.attachmentNumbers) != 1 || draft.attachmentNumbers[0] != survivor.number {
		t.Fatal("deleting a preceding attachment changed the survivor number or left a stale MIME index")
	}
	faiRunOK(t, f, []byte{ropCreateAttachment, 4, message, attachment})
	pending := f.state.mailbox.objects[f.handles[attachment]].attachment.draft
	faiRunOK(t, f, streamOpenWire(attachment, stream, 2, attachDataBinary))
	faiRunOK(t, f, binary.LittleEndian.AppendUint64([]byte{ropSetStreamSize, 4, stream}, 64))
	if draft.incompleteAttachments != 1 {
		t.Fatal("pending attachment must be incomplete before deletion")
	}
	faiRunOK(t, f, append32([]byte{ropDeleteAttachment, 4, message}, pending.number))
	if draft.incompleteAttachments != 0 || len(draft.attachmentNumbers) != 1 {
		t.Fatal("deleting an incomplete attachment changed the MIME rows or left the draft unsavable")
	}
}

func decodeStreamRead(t *testing.T, response []byte, slot byte) []byte {
	t.Helper()
	if len(response) < 8 || !bytes.HasPrefix(response, ropError(0x2c, slot, 0)) ||
		int(binary.LittleEndian.Uint16(response[6:8])) != len(response)-8 {
		t.Fatal("malformed ReadStream response")
	}
	return response[8:]
}

func TestAttachmentStreamThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	message := &box.objects[0]
	payload := bytes.Repeat([]byte{0, 0xff, 0x80, 0x10, 0x45}, 19000)
	body := base64.StdEncoding.EncodeToString(payload)
	message.MIME = []byte("Subject: stream fixture\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=stream.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n" + body)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("stream fixture unavailable", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	leaseBaseline := store.allocator.contents.used
	open := streamOpenWire(3, 2, 0, attachDataBinary)
	read := streamReadWire(2, 15000)
	packed := append32(streamReadWire(2, 0xbabe), 1<<31)
	for _, tc := range []struct {
		request []byte
		code    uint32
	}{
		{streamOpenWire(1, 2, 0, attachDataBinary), NotSupported},
		{streamOpenWire(3, 2, 0, 0x3701010d), PropertyNotFound},
		{streamOpenWire(3, 2, 1, attachDataBinary), 0x80030005},
		{streamOpenWire(3, 2, 2, attachDataBinary), 0x80030005},
		{streamOpenWire(3, 2, 3, attachDataBinary), 0x80070057},
	} {
		before := box.mimeReads
		got, _ := f.run(tc.request)
		if !bytes.Equal(got, ropError(0x2b, 2, tc.code)) || box.mimeReads != before {
			t.Fatal("refused Stream open accessed MIME or returned an unexpected result")
		}
	}
	before := box.mimeReads
	bodyResponse, _ := f.execute(append(bytes.Clone(open), packed[:8]...), 65535)
	if !bytes.Equal(bodyResponse, executeError(0x80070057, 65535)) || box.mimeReads != before {
		t.Fatal("truncated packed read executed the batch prefix")
	}
	bodyResponse, _ = f.execute(append(bytes.Clone(open), append32(streamReadWire(2, 0xbabe), 1<<31+1)...), 65535)
	if !bytes.Equal(bodyResponse, Failure(0x000004b6)) || box.mimeReads != before {
		t.Fatal("oversize maximum reference executed the batch prefix")
	}
	quota, err := store.reserveContentsMemory(maxStoreContentsMemory - 2*maxMailProjectionBytes + 1)
	if err != nil {
		t.Fatal("could not arrange stream quota", err)
	}
	got, _ := f.run(open)
	quota.release()
	if !bytes.Equal(got, ropError(0x2b, 2, NotSupported)) || box.mimeReads != before {
		t.Fatal("stream quota admitted a full MIME read")
	}

	size := uint32(len(payload))
	got = faiRunOK(t, f, open)
	if !bytes.Equal(got, append32(ropError(0x2b, 2, 0), size)) {
		t.Fatal("OpenStream size")
	}
	lock := []byte{ropLockRegionStream, 4, 2}
	lock = binary.LittleEndian.AppendUint64(lock, 1)
	lock = binary.LittleEndian.AppendUint64(lock, 4)
	lock = append32(lock, 1)
	unlock := bytes.Clone(lock)
	unlock[0] = ropUnlockRegionStream
	progress := []byte{ropProgress, 4, 1, 1}
	optionalOps := append(append(append(bytes.Clone(lock), unlock...), progress...), 0x5e, 4, 2)
	wantOptional := append(ropError(ropLockRegionStream, 2, 0), ropError(ropUnlockRegionStream, 2, 0)...)
	wantOptional = append(wantOptional, ropError(ropProgress, 1, NotSupported)...)
	wantOptional = append(wantOptional, append32(ropError(0x5e, 2, 0), size)...)
	if got, counts := f.run(optionalOps); !bytes.Equal(got, wantOptional) || counts.errors != 1 {
		t.Fatalf("optional operations aborted or changed the stream: %x", got)
	}
	for _, request := range [][]byte{lock, unlock, progress} {
		body, _ := f.execute(append([]byte{1, 4, 2}, request[:len(request)-1]...), 65535)
		_, streamStillOpen := f.state.mailbox.objects[f.handles[2]]
		if !bytes.Equal(body, executeError(0x80070057, 65535)) || !streamStillOpen {
			t.Fatal("malformed optional operation executed the Release prefix")
		}
	}
	got, _ = f.run([]byte{ropWriteAndCommitStream, 4, 2, 1, 0, 0xff})
	if !bytes.Equal(got, ropError(ropWriteAndCommitStream, 2, 0x80030005)) {
		t.Fatal("WriteAndCommitStream accepted a read-only stream")
	}
	got = faiRunOK(t, f, []byte{0x5e, 4, 2})
	if !bytes.Equal(got, append32(ropError(0x5e, 2, 0), size)) {
		t.Fatal("GetStreamSize size")
	}
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(2, 0)), 2); len(got) != 0 {
		t.Fatal("zero-byte read returned content")
	}
	maxOutput := uint32(10 + 4*len(f.handles) + 8)
	bodyResponse, _ = f.execute(read, maxOutput)
	wantFailure := append(ropError(0x2c, 2, tableBufferTooSmall), 0, 0)
	if !bytes.Equal(storeResponse(t, bodyResponse).Operations, wantFailure) {
		t.Fatal("short output did not distinguish no room from EOF")
	}
	bodyResponse, _ = f.execute(read, maxOutput+7)
	result := append([]byte(nil), decodeStreamRead(t, storeResponse(t, bodyResponse).Operations, 2)...)
	if len(result) != 7 {
		t.Fatal("output budget did not govern the first chunk")
	}

	got = faiRunOK(t, f, streamOpenWire(3, 1, 0, attachDataBinary))
	if !bytes.Equal(got, append32(ropError(0x2b, 1, 0), size)) {
		t.Fatal("second Stream open failed")
	}
	faiRunOK(t, f, []byte{1, 4, 3}) // Streams have independent handles and cursors.
	before = box.mimeReads
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(1, 4)), 1); !bytes.Equal(got, payload[:4]) {
		t.Fatal("second stream shared the first cursor")
	}
	foreign := bytes.Clone(read)
	foreign[1] = 5
	got, _ = f.run(foreign)
	if !bytes.Equal(got, append(ropError(0x2c, 2, accessDenied), 0, 0)) {
		t.Fatal("foreign logon was allowed to read a stream")
	}
	if got = decodeStreamRead(t, faiRunOK(t, f, packed), 2); len(got) != 32768-10-4*len(f.handles)-8 {
		t.Fatal("packed read exceeded or underfilled the initial buffer")
	}
	result = append(result, got...)
	for len(result) < len(payload) {
		part := decodeStreamRead(t, faiRunOK(t, f, read), 2)
		if len(part) == 0 {
			t.Fatal("premature EOF")
		}
		result = append(result, part...)
	}
	if !bytes.Equal(result, payload) || len(decodeStreamRead(t, faiRunOK(t, f, read), 2)) != 0 || box.mimeReads != before {
		t.Fatal("ReadStream did not reproduce exact data, EOF, or expected MIME reads")
	}

	for _, tc := range []struct {
		origin byte
		offset int64
		pos    uint64
		code   uint32
	}{
		{2, -3, uint64(size - 3), 0},
		{1, 2, uint64(size - 1), 0},
		{1, math.MaxInt64, 0, 0x80030019},
		{0, -1, 0, 0x80030019},
		{3, 0, 0, 0x80030057},
		{1, 0, uint64(size - 1), 0},
	} {
		wire := streamSeekWire(2, tc.origin, tc.offset)
		got, _ = f.run(wire)
		want := ropError(0x2e, 2, tc.code)
		if tc.code == 0 {
			want = binary.LittleEndian.AppendUint64(want, tc.pos)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("SeekStream result")
		}
	}
	if got := decodeStreamRead(t, faiRunOK(t, f, read), 2); !bytes.Equal(got, payload[len(payload)-1:]) {
		t.Fatal("seek position did not govern the next read")
	}
	faiRunOK(t, f, streamSeekWire(2, 2, 10))
	if got := decodeStreamRead(t, faiRunOK(t, f, read), 2); len(got) != 0 {
		t.Fatal("seek beyond EOF returned data in read-only stream")
	}

	f.handles = append(f.handles, 0xffffffff)
	const clone = 4
	faiRunOK(t, f, streamSeekWire(1, 0, 7))
	before = box.mimeReads
	used := store.allocator.contents.used
	for _, tc := range []struct {
		wire []byte
		code uint32
	}{
		{[]byte{ropCloneStream, 4, 0, clone}, NotSupported},
		{[]byte{ropCloneStream, 5, 1, clone}, accessDenied},
	} {
		if got, _ := f.run(tc.wire); !bytes.Equal(got, ropError(ropCloneStream, clone, tc.code)) || f.handles[clone] != 0xffffffff {
			t.Fatal("rejected clone changed its destination")
		}
	}
	bodyResponse, _ = f.execute([]byte{ropCloneStream, 4, 1, clone, ropCloneStream, 4, 1, clone + 1}, 65535)
	if !bytes.Equal(bodyResponse, executeError(0x80070057, 65535)) || f.handles[clone] != 0xffffffff {
		t.Fatal("malformed clone did not reject the complete ROP buffer")
	}
	cloneWire := []byte{ropCloneStream, 4, 1, clone}
	if got := faiRunOK(t, f, cloneWire); !bytes.Equal(got, ropError(ropCloneStream, clone, 0)) ||
		f.handles[clone] == f.handles[1] || box.mimeReads != before || store.allocator.contents.used != used {
		t.Fatal("clone did not allocate a distinct handle over the same MIME buffer")
	}
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(clone, 3)), clone); !bytes.Equal(got, payload[7:10]) {
		t.Fatal("clone did not inherit the cursor")
	}
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(1, 4)), 1); !bytes.Equal(got, payload[7:11]) {
		t.Fatal("clone moved the source cursor")
	}
	faiRunOK(t, f, []byte{1, 4, 1})
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(clone, 3)), clone); !bytes.Equal(got, payload[10:13]) ||
		store.allocator.contents.used != used {
		t.Fatal("source release discarded the shared buffer while the clone was open")
	}
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	got, _ = f.run(read)
	if !bytes.Equal(got, append(ropError(0x2c, 2, 0x80040109), 0, 0)) {
		t.Fatal("stale stream exposed bytes")
	}
	got, _ = f.run(streamReadWire(clone, 3))
	if !bytes.Equal(got, append(ropError(0x2c, clone, 0x80040109), 0, 0)) {
		t.Fatal("stale clone exposed bytes")
	}
	faiRunOK(t, f, []byte{1, 4, clone})
	if store.allocator.contents.used >= used {
		t.Fatal("the final clone did not release its buffer")
	}
	message.Parent = "another-folder"
	got, _ = f.run(read)
	if !bytes.Equal(got, append(ropError(0x2c, 2, PropertyNotFound), 0, 0)) {
		t.Fatal("stream escaped the message's original folder")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	got, _ = f.run(read)
	if !bytes.Equal(got, append(ropError(0x2c, 2, invalidObject), 0, 0)) {
		t.Fatal("released stream remained readable")
	}
	faiRunOK(t, f, []byte{1, 4, 0})
	if store.allocator.contents.used != leaseBaseline {
		t.Fatal("stream memory was not released")
	}
}

func TestMIMEStreamLocksAcrossContexts(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.session = &testSession{&attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}}
	base.objects[0].MIME = []byte("Content-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=region.bin\r\n\r\nabcdefghi")
	binding := f.state.mailbox.objects[f.handles[0]].binding
	fid := f.fids[Inbox]
	snapshot, err := binding.store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("observe attachment message:", err)
	}
	open := func(f *ostFixture) {
		faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
		faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
		faiRunOK(t, f, streamOpenWire(3, 2, 0, attachDataBinary))
	}
	other := *f
	other.state = &contextState{mailbox: newMailboxObjects(f.transport.stores), codePage: 1252}
	t.Cleanup(other.state.mailbox.close)
	logon, err := other.state.mailbox.addLogon(4, binding)
	if err != nil {
		t.Fatal(err)
	}
	other.handles = []uint32{logon, 0xffffffff, 0xffffffff, 0xffffffff}
	faiRunOK(t, &other, folderOperation(fid, 4, 0, 1))
	open(f)
	open(&other)
	lock := func(rop byte, slot byte, offset, size uint64, flags uint32) []byte {
		wire := binary.LittleEndian.AppendUint64([]byte{rop, 4, slot}, offset)
		return append32(binary.LittleEndian.AppendUint64(wire, size), flags)
	}
	check := func(f *ostFixture, wire []byte, code uint32) {
		t.Helper()
		want := ropError(wire[0], wire[2], code)
		if wire[0] == 0x2c {
			want = append(want, 0, 0)
		}
		if got, _ := f.run(wire); !bytes.Equal(got, want) {
			t.Fatalf("got %x, want %x", got, want)
		}
	}
	check(f, lock(ropLockRegionStream, 2, math.MaxUint64, 2, 1), 0x80070057)
	check(f, lock(ropLockRegionStream, 2, 1, 4, 1), 0)
	if got := decodeStreamRead(t, faiRunOK(t, &other, streamReadWire(2, 2)), 2); string(got) != "ab" {
		t.Fatalf("write lock blocked reading: %q", got)
	}
	check(&other, lock(ropLockRegionStream, 2, 2, 1, 2), accessDenied)
	check(f, lock(ropUnlockRegionStream, 2, 1, 4, 1), 0)
	check(f, lock(ropLockRegionStream, 2, 1, 4, 2), 0)
	check(&other, lock(ropUnlockRegionStream, 2, 1, 4, 2), 0)
	check(&other, streamReadWire(2, 3), accessDenied)
	faiRunOK(t, f, []byte{ropCloneStream, 4, 2, 3})
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(3, 3)), 3); string(got) != "abc" {
		t.Fatalf("owner's clone could not read: %q", got)
	}
	f.state.mailbox.streamLockSession.activity.Store(int64(time.Since(streamLockClock) - 31*time.Second))
	check(&other, lock(ropLockRegionStream, 2, 2, 1, 2), 0)
	check(f, streamReadWire(2, 2), streamLockLost)
	faiRunOK(t, &other, []byte{1, 4, 2})
	check(f, streamReadWire(2, 2), streamLockLost)
	check(f, lock(ropUnlockRegionStream, 3, 1, 4, 2), 0)
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(2, 2)), 2); string(got) != "ab" {
		t.Fatalf("stream did not recover after unlock: %q", got)
	}
}
