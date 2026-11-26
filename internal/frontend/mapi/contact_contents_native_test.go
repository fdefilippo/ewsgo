// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func qualifyNativePersonalContact(t *testing.T, ctx context.Context, native core.Session, fids [specialFolderCount]uint64, success func([]byte) []byte, runROP func([]byte, uint32) []byte) {
	t.Helper()
	inbox, trash := fids[Inbox], fids[DeletedItems]
	box := native.Mailbox()
	folder, err := box.(core.SpecialFolderReader).SpecialFolder(ctx, core.ContactsFolder)
	if err != nil {
		t.Fatal("native Contacts unavailable")
	}
	reader, writer := box.(core.ContactReader), box.(core.ContactWriter)
	name := "EWSGo contact fixture " + time.Now().UTC().Format("20060102T150405.000000000")
	const firstEmail = "personal-one@example.invalid"
	const thirdEmail = "personal-three@example.invalid"
	contact, err := writer.CreateContact(ctx, folder.ID, core.ContactPatch{
		"DisplayName": name, "GivenName": "EWSGo", "Body": "Initial contact note",
		"EmailAddress1": firstEmail, "EmailAddress3": thirdEmail,
	})
	if err != nil {
		t.Fatal("could not seed the native contact")
	}
	id := contact.Identity.ID
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		cleanup, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		current, err := reader.GetContact(cleanup, folder.ID, id)
		if err != nil || writer.TrashContact(cleanup, current) != nil {
			t.Error("could not clean up the synthetic contact")
		}
	}()
	success(folderOperation(inbox, 4, 0, 1))
	wire := success(folderPropertiesOperation(4, 1, 1, 0x36d10102))
	if len(wire) != 55 || binary.LittleEndian.Uint16(wire[7:9]) != 46 {
		t.Fatal("missing default Contacts EntryID")
	}
	wire = success(conversionOperation(0x44, wire[31:]))
	if len(wire) != 14 {
		t.Fatal("could not convert Contacts EntryID")
	}
	fid := binary.LittleEndian.Uint64(wire[6:])
	success(folderOperation(fid, 4, 0, 1))
	readAggregates := func() (uint64, uint32) { return readNativeFolderChangeIndicators(t, success) }
	commit, deleted := readAggregates()
	if again, count := readAggregates(); commit == 0 || again != commit || count != deleted {
		t.Fatal("unchanged native Contacts lost stable change indicators")
	}
	success(folderOperation(fids[IPMSubtree], 4, 0, 1))
	hierarchy := success([]byte{4, 4, 1, 2, 4})
	if len(hierarchy) != 10 {
		t.Fatal("unexpected hierarchy table response")
	}
	tags := []uint32{0x67480014, 0x670a0040, 0x670b0003}
	success(tableColumnsWire(2, tags...))
	found := false
	for remaining := binary.LittleEndian.Uint32(hierarchy[6:]); remaining > 0 && !found; {
		query := success(tableQueryWire(2, 0, 1, uint16(min(remaining, 20))))
		rows := decodeTableRows(t, query, tags)
		if len(rows) == 0 || uint32(len(rows)) > remaining {
			t.Fatal("inconsistent hierarchy table count")
		}
		for _, row := range rows {
			if row[0] == fid {
				if row[1] != commit || row[2] != deleted {
					t.Fatal("hierarchy row differs from direct native Contacts properties")
				}
				found = true
				break
			}
		}
		remaining -= uint32(len(rows))
	}
	if !found {
		t.Fatal("Contacts row absent from hierarchy")
	}
	success([]byte{1, 4, 2})
	success(folderOperation(fid, 4, 0, 1))
	readMID := func() uint64 {
		tags := []uint32{0x674a0014, 0x001a001f, 0x3001001f}
		opened := success([]byte{5, 4, 1, 2, 0x10})
		if len(opened) != 10 {
			t.Fatal("unexpected contact contents response")
		}
		count := binary.LittleEndian.Uint32(opened[6:])
		success(tableColumnsWire(2, tags...))
		for remaining := count; remaining != 0; {
			rows := decodeTableRows(t, success(tableQueryWire(2, 0, 1, uint16(min(remaining, 20)))), tags)
			if len(rows) == 0 || uint32(len(rows)) > remaining {
				t.Fatal("inconsistent contact table count")
			}
			for _, row := range rows {
				if row[1] == "IPM.Contact" && row[2] == name {
					success([]byte{1, 4, 2})
					return row[0].(uint64)
				}
			}
			remaining -= uint32(len(rows))
		}
		t.Fatal("synthetic contact absent from table")
		return 0
	}
	mid := readMID()
	success(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 0))
	lids := []uint32{0x8028, 0x8029, 0x8082, 0x8083, 0x8085, 0x80a3}
	kinds := []uint16{0x1003, 3, 0x001f, 0x001f, 0x0102, 0x001f}
	var names []propertyName
	for _, lid := range lids {
		prop, err := corePropertyName(core.Key{Set: contactAddressSet, ID: lid})
		if err != nil {
			t.Fatal("invalid contact email property set")
		}
		names = append(names, prop)
	}
	ids := propertyResponseIDs(t, ROPBuffer{Operations: success(propertyIDsOperation(4, 3, 2, names...))})
	var emailTags []uint32
	for i, id := range ids {
		emailTags = append(emailTags, uint32(id)<<16|uint32(kinds[i]))
	}
	readEmail := success(folderPropertiesOperation(4, 3, 1, emailTags...))
	prefix := append([]byte{2, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 5, 0, 0, 0}, contentsUnicode("SMTP")...)
	prefix = append(prefix, contentsUnicode(firstEmail)...)
	if len(readEmail) < 7+len(prefix)+2 || readEmail[6] != 0 || !bytes.Equal(readEmail[7:7+len(prefix)], prefix) {
		t.Fatal("opened contact email slots, mask or SMTP properties differ")
	}
	payload := readEmail[7+len(prefix):]
	entryLen := int(binary.LittleEndian.Uint16(payload))
	if 2+entryLen >= len(payload) || !bytes.Equal(payload[2+entryLen:], contentsUnicode(thirdEmail)) {
		t.Fatal("opened contact one-off EntryID or third address is invalid")
	}
	if gotName, address, err := importEntryAddress(payload[2:2+entryLen], nil); err != nil || gotName != name || address != firstEmail {
		t.Fatal("opened contact one-off EntryID did not resolve to its SMTP address")
	}
	readNote := func(value string) {
		wire := success(folderPropertiesOperation(4, 3, 1, 0x3a06001f, 0x1000001f))
		if !bytes.Equal(wire[7:], append(contentsUnicode("EWSGo"), contentsUnicode(value)...)) {
			t.Fatal("opened contact data differs from native SOAP")
		}
	}
	readNote("Initial contact note")
	config := fullContentsConfig()
	config.flags = 0x21
	run := func(op []byte) ([]byte, storeResults) { return success(op), storeResults{} }
	success(contentsConfigureOperation(config, nil))
	initial := drainFX(t, run, 2, 16384)
	state := contentsFourTestSets(t, initial)
	if !containsContactFX(t, initial, 0x3001001f, name) || !containsContactFX(t, initial, 0x1000001f, "Initial contact note") {
		t.Fatal("initial cache transfer omitted the synthetic contact")
	}
	assertContactEmailFX(t, initial, names[0].Set, []int32{0, 2}, 5, 0x80a3, thirdEmail)
	success([]byte{1, 4, 2})
	const secondEmail = "personal-two@example.invalid"
	contact, err = writer.UpdateContact(ctx, contact, core.ContactPatch{"Body": "Updated contact note", "EmailAddress2": secondEmail, "EmailAddress3": ""})
	if err != nil || readMID() != mid {
		t.Fatal("native update did not preserve contact MID")
	}
	updatedCommit, count := readAggregates()
	if updatedCommit <= commit || count != deleted {
		t.Fatal("native contact update did not advance the folder commit time alone")
	}
	readNote("Updated contact note")
	success(contentsConfigureOperation(config, nil))
	uploadContentsWireTestState(t, run, state)
	delta := drainFX(t, run, 2, 16384)
	if !containsContactFX(t, delta, 0x1000001f, "Updated contact note") {
		t.Fatal("cache delta omitted the native update")
	}
	assertContactEmailFX(t, delta, names[0].Set, []int32{0, 1}, 3, 0x8093, secondEmail)
	secondName, err := corePropertyName(core.Key{Set: contactAddressSet, ID: 0x8093})
	if err != nil {
		t.Fatal("invalid contact second email property")
	}
	secondID := propertyResponseIDs(t, ROPBuffer{Operations: success(propertyIDsOperation(4, 3, 0, secondName))})[0]
	if secondID < 0x8001 {
		t.Fatal("cache transfer did not bind the second address property")
	}
	success([]byte{1, 4, 3})
	modify := func(note string) []byte {
		return faiSetOperation(3, faiSetRequest{{tag: 0x1000001f, value: core.Text(note)}})
	}
	const onlineNote = "MAPI staged contact edit"
	batch := success(append(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 1), modify(onlineNote)...))
	if len(batch) < openMessageMinimum+8 || !bytes.Equal(batch[len(batch)-8:], propertyProblemsResponse(0x0a, 3, nil)) {
		t.Fatal("batched open and property staging failed")
	}
	readNote(onlineNote)
	remove := success(deletePropertiesOperation(ropDeleteProperties, 3, uint32(secondID)<<16|0x001f))
	if !bytes.Equal(remove, propertyProblemsResponse(ropDeleteProperties, 3, nil)) {
		t.Fatal("MAPI could not stage the address removal")
	}
	before, err := reader.GetContact(ctx, folder.ID, id)
	if err != nil || before.Notes != "Updated contact note" || before.Emails[1] != secondEmail {
		t.Fatal("MAPI staged fields reached Zimbra before SaveChanges")
	}
	save := []byte{0x0c, 4, 1, 3, 2}
	oldChangeKey := success(folderPropertiesOperation(4, 3, 1, 0x65e20102))
	ack := success(save)
	if len(ack) != 15 || binary.LittleEndian.Uint64(ack[7:]) != mid {
		t.Fatal("contact SaveChanges did not return its stable MID")
	}
	newChangeKey := success(folderPropertiesOperation(4, 3, 1, 0x65e20102))
	if len(oldChangeKey) < 31 || len(newChangeKey) < 31 || oldChangeKey[6] != 0 || newChangeKey[6] != 0 || bytes.Equal(oldChangeKey, newChangeKey) {
		t.Fatal("native contact SaveChanges did not advance the MAPI ChangeKey")
	}
	current, err := reader.GetContact(ctx, folder.ID, id)
	if err != nil || current.Notes != onlineNote || current.GivenName != "EWSGo" || current.Emails[0] != firstEmail || current.Emails[1] != "" {
		t.Fatal("native contact merge after MAPI SaveChanges differs from the staged edit")
	}
	success(modify("Stale pending update"))
	contact, err = writer.UpdateContact(ctx, current, core.ContactPatch{"JobTitle": "Native concurrent edit"})
	if err != nil || !bytes.Equal(runROP(save, 65535), ropError(0x0c, 1, 0x80040109)) {
		t.Fatal("a newer native contact revision did not reject the stale MAPI save")
	}
	current, err = reader.GetContact(ctx, folder.ID, id)
	if err != nil || current.Notes != onlineNote || current.JobTitle != "Native concurrent edit" {
		t.Fatal("stale MAPI save changed the native contact")
	}
	success([]byte{1, 4, 3})
	success(folderOperation(trash, 4, 0, 3))
	if moved := success(moveCopyWire(4, 1, 3, 0, 0, mid)); len(moved) != 7 || moved[6] != 0 {
		t.Fatal("MAPI move to Trash did not complete the synthetic contact")
	}
	if _, err = reader.GetContact(ctx, folder.ID, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("synthetic contact remained in Contacts")
	}
	cleaned = true
	if commit, count := readAggregates(); commit <= updatedCommit || count != deleted+1 {
		t.Fatal("contact Trash did not advance both folder change indicators")
	}
	state = contentsFourTestSets(t, delta)
	success([]byte{1, 4, 2})
	success(contentsConfigureOperation(config, nil))
	uploadContentsWireTestState(t, run, state)
	removed := drainFX(t, run, 2, 16384)
	if contentsFourTestSets(t, removed).given.contains(state.given.replicas[0].replica, folderGlobalCount(mid)) {
		t.Fatal("native Trash left the contact in the cache checkpoint")
	}
	success([]byte{1, 4, 2})
	qualifyNativeContactCreate(t, ctx, native, fid, trash, folder.ID, emailTags[3], success, runROP)
}

func readNativeFolderChangeIndicators(t *testing.T, success func([]byte) []byte) (uint64, uint32) {
	t.Helper()
	wire := success(folderPropertiesOperation(4, 1, 0, 0x670a0040, 0x670b0003))
	if len(wire) != 19 || wire[6] != 0 {
		t.Fatal("native folder change indicators unavailable")
	}
	return binary.LittleEndian.Uint64(wire[7:]), binary.LittleEndian.Uint32(wire[15:])
}

func qualifyNativeContactCreate(t *testing.T, ctx context.Context, native core.Session, fid, trash uint64, parent core.ID, emailTag uint32, success func([]byte) []byte, runROP func([]byte, uint32) []byte) {
	t.Helper()
	reader := native.Mailbox().(core.ContactReader)
	writer := native.Mailbox().(core.ContactWriter)
	name := "EWSGo contact create " + time.Now().UTC().Format("20060102T150405.000000000")
	find := func(ctx context.Context) ([]core.Contact, error) {
		contacts, err := reader.ListContacts(ctx, parent)
		var matches []core.Contact
		for _, contact := range contacts {
			if contact.DisplayName == name {
				matches = append(matches, contact)
			}
		}
		return matches, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		items, err := find(cleanup)
		if err != nil {
			t.Error("could not verify synthetic contact cleanup")
		}
		for _, item := range items {
			if writer.TrashContact(cleanup, item) != nil {
				t.Error("could not clean up a synthetic contact")
			}
		}
	}()
	create := append(binary.LittleEndian.AppendUint64([]byte{0x06, 4, 1, 3, 0xff, 0x0f}, fid), 0)
	displayName := faiWireProperty{tag: 0x3001001f, value: core.Text(name)}
	save := []byte{0x0c, 4, 1, 3, 2}
	if reply := success(create); !bytes.Equal(reply, append(ropError(0x06, 3, 0), 0)) {
		t.Fatal("new contact advertised a MID before being saved")
	}
	if reply := success(folderPropertiesOperation(4, 3, 1, 0x001a001f)); !bytes.Equal(reply[7:], contentsUnicode("IPM.Note")) {
		t.Fatal("new contact handle has the wrong initial message class")
	}
	wrongClass := faiWireProperty{tag: 0x001a001f, value: core.Text("IPM.DistList")}
	set := success(faiSetOperation(3, faiSetRequest{wrongClass, displayName}))
	expected := propertyProblemsResponse(0x0a, 3, []faiPropertyProblem{{0, wrongClass.tag, NotSupported}})
	if !bytes.Equal(set, expected) || !bytes.Equal(runROP(save, 65535), ropError(0x0c, 1, 0x80070057)) {
		t.Fatal("contact creation accepted an unsupported class")
	}
	success([]byte{1, 4, 3})

	const address = "mapi-contact-new@example.invalid"
	props := faiSetRequest{
		{tag: 0x001a001f, value: core.Text(contactClass)}, displayName,
		{tag: emailTag, value: core.Text(address)},
		{tag: 0x1000001f, value: core.Text("Created from MAPI")},
	}
	want := append(append(ropError(0x06, 3, 0), 0), propertyProblemsResponse(0x0a, 3, nil)...)
	if got := success(append(bytes.Clone(create), faiSetOperation(3, props)...)); !bytes.Equal(got, want) {
		t.Fatal("batched create and property staging failed")
	}
	wire := success(folderPropertiesOperation(4, 3, 1, 0x3001001f, emailTag))
	if !bytes.Equal(wire[7:], append(contentsUnicode(name), contentsUnicode(address)...)) {
		t.Fatal("unsaved contact did not expose its staged properties")
	}
	if items, err := find(ctx); err != nil || len(items) != 0 {
		t.Fatal("staged contact reached Zimbra before SaveChanges")
	}
	ack := success(save)
	if len(ack) != 15 || binary.LittleEndian.Uint64(ack[7:]) == 0 || !bytes.Equal(ack, success(save)) {
		t.Fatal("contact SaveChanges failed to return a stable MID")
	}
	items, err := find(ctx)
	if err != nil || len(items) != 1 || items[0].Emails[0] != address || items[0].Notes != "Created from MAPI" {
		t.Fatal("Zimbra does not contain exactly one contact with the saved fields")
	}
	mid := binary.LittleEndian.Uint64(ack[7:])
	success([]byte{1, 4, 3})
	success(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 0))
	wire = success(folderPropertiesOperation(4, 3, 1, 0x001a001f, 0x3001001f))
	if !bytes.Equal(wire[7:], append(contentsUnicode(contactClass), contentsUnicode(name)...)) {
		t.Fatal("the saved contact did not reopen with the correct ID and class")
	}
	success([]byte{1, 4, 3})
	success(folderOperation(trash, 4, 0, 3))
	if moved := success(moveCopyWire(4, 1, 3, 0, 0, mid)); len(moved) != 7 || moved[6] != 0 {
		t.Fatal("the newly created contact could not be moved to Trash")
	}
	if _, err := reader.GetContact(ctx, parent, items[0].Identity.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("the newly created contact remained in Contacts after the move")
	}
}

func assertContactEmailFX(t *testing.T, raw []byte, set [16]byte, slots []int32, mask int32, lid uint32, address string) {
	t.Helper()
	var capture, slotMatch, maskMatch, addressMatch bool
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x40150003 || token.tag == 0x40100003 { // IncrSyncMessage / next IncrSyncChange.
			if capture && slotMatch && maskMatch && addressMatch {
				return
			}
			capture = token.tag == 0x40150003
			slotMatch, maskMatch, addressMatch = false, false, false
		}
		if !capture || token.set != set {
			continue
		}
		switch token.lid {
		case 0x8028:
			slotMatch = reflect.DeepEqual(token.value, slots)
		case 0x8029:
			maskMatch = token.value == mask
		case lid:
			addressMatch = token.value == address
		}
	}
	if !slotMatch || !maskMatch || !addressMatch {
		t.Fatal("cache transfer did not carry matching contact electronic address properties")
	}
}
