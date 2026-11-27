// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"ewsgo/internal/core"
)

type placementFixture struct {
	fixtureMailbox
	writes int
}

func (*placementFixture) Root() core.ID { return "root" }
func (f *placementFixture) Query(_ context.Context, _ core.Query) (core.Page, error) {
	return core.Page{Objects: []core.Object{f.objects["trash"]}}, nil
}
func (f *placementFixture) MoveMailMessage(ctx context.Context, id core.ID, rev core.Revision, parent core.ID) (core.Object, error) {
	return f.place(ctx, id, rev, parent, id)
}
func (f *placementFixture) CopyMailMessage(ctx context.Context, id core.ID, rev core.Revision, parent core.ID) (core.Object, error) {
	return f.place(ctx, id, rev, parent, "copy")
}
func (f *placementFixture) TrashMailMessage(ctx context.Context, id core.ID, rev core.Revision) (core.Object, error) {
	return f.place(ctx, id, rev, "trash", id)
}
func (f *placementFixture) RestoreMailMessage(ctx context.Context, id core.ID, rev core.Revision, parent core.ID) (core.Object, error) {
	return f.MoveMailMessage(ctx, id, rev, parent)
}
func (f *placementFixture) SoftDeleteMailMessage(ctx context.Context, id core.ID, rev core.Revision) error {
	item, err := f.Get(ctx, id)
	if err != nil || item.Revision != rev {
		return core.ErrConflict
	}
	f.writes++
	delete(f.objects, id)
	return nil
}
func (f *placementFixture) place(ctx context.Context, source core.ID, revision core.Revision, parent core.ID, id core.ID) (core.Object, error) {
	item, err := f.Get(ctx, source)
	if err != nil || item.Revision != revision {
		return core.Object{}, core.ErrConflict
	}
	f.writes++
	item.ID, item.Parent, item.Revision = id, parent, revision+1
	f.objects[id] = item
	return item, nil
}
func (f *placementFixture) CreateMailFolder(_ context.Context, p core.MailFolderPlacement) (core.Object, error) {
	return f.UpdateMailFolder(context.Background(), core.Object{ID: "new", Kind: core.Folder}, p)
}
func (f *placementFixture) UpdateMailFolder(_ context.Context, old core.Object, p core.MailFolderPlacement) (core.Object, error) {
	f.writes++
	old.Parent, old.Revision = p.Parent, old.Revision+1
	old.Properties = core.Properties{core.DisplayName: core.Text(p.Name)}
	f.objects[old.ID] = old
	return old, nil
}

func serveWriteTest(t *testing.T, h *Handler, session core.Session, op node, want string) node {
	t.Helper()
	w := httptest.NewRecorder()
	h.Serve(w, httptest.NewRequest("POST", "/EWS/Exchange.asmx", bytes.NewReader(envelope(op))), session)
	n, err := parse(w.Body.Bytes())
	if err != nil {
		t.Fatalf("parse response for %s: %v", op.XMLName.Local, err)
	}
	code := n.find(messages, "ResponseCode")
	if code == nil {
		t.Fatalf("%s: missing ResponseCode", op.XMLName.Local)
	}
	if code.Text != want {
		t.Fatalf("%s: want %s, got %s", op.XMLName.Local, want, code.Text)
	}
	return n
}

func TestItemPlacementWireUsesFolderIdentityAndRevision(t *testing.T) {
	msg := core.Object{ID: "message", Parent: "inbox", Kind: core.Message, Revision: 2}
	box := &placementFixture{fixtureMailbox: fixtureMailbox{objects: map[core.ID]core.Object{msg.ID: msg}}}
	h := New()
	s := &testSession{box: box}
	scope := s.Identity().Scope
	ref := func(id, key string) node {
		return at(at(el(types, "ItemId", ""), "Id", id), "ChangeKey", key)
	}
	originalID := h.itemID(scope, msg)
	original := ref(originalID, h.changeKey(scope, msg))
	target := el(messages, "ToFolderId", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, "target")))
	move := func(op string, ref node, returns string) node {
		return el(messages, op, "", target, el(messages, "ItemIds", "", ref), el(messages, "ReturnNewItemIds", returns))
	}
	remove := func(mode string, ref node) node {
		return at(el(messages, "DeleteItem", "", el(messages, "ItemIds", "", ref)), "DeleteType", mode)
	}
	serveWriteTest(t, h, s, move("CopyItem", ref(originalID, "stale"), "true"), "ErrorIrresolvableConflict")
	serveWriteTest(t, h, s, remove("HardDelete", original), "ErrorCannotDeleteObject")
	if box.writes != 0 {
		t.Fatal("a rejected action wrote")
	}
	copied := serveWriteTest(t, h, s, move("CopyItem", original, "true"), "NoError")
	moved := serveWriteTest(t, h, s, move("MoveItem", original, "true"), "NoError")
	copyID, moveID := copied.find(types, "ItemId"), moved.find(types, "ItemId")
	if copyID == nil || moveID == nil || moveID.attr("Id") == originalID ||
		moveID.attr("Id") == copyID.attr("Id") || moveID.attr("ChangeKey") == original.attr("ChangeKey") {
		t.Fatal("copy or move yielded an incorrect wire identity")
	}
	serveWriteTest(t, h, s, move("MoveItem", original, "true"), "ErrorItemNotFound")
	serveWriteTest(t, h, s, remove("MoveToDeletedItems", *moveID), "NoError")
	trashed := box.objects["message"]
	if trashed.Parent != "trash" || box.objects["copy"].Parent != "target" {
		t.Fatal("wrong placement after deletion")
	}
	serveWriteTest(t, h, s, remove("SoftDelete", ref(h.itemID(scope, trashed), "")), "NoError")
	if _, exists := box.objects["message"]; exists || box.writes != 4 {
		t.Fatal("wrong active membership after soft delete")
	}
}

func TestItemPlacementRequiresOperationCapability(t *testing.T) {
	item := core.Object{ID: "message", Kind: core.Message, Parent: "inbox", Revision: 1}
	box := &placementFixture{fixtureMailbox: fixtureMailbox{objects: map[core.ID]core.Object{item.ID: item}}}
	placement := &testSession{box: struct {
		core.Mailbox
		core.MailMessagePlacementWriter
	}{box, box}}
	softOnly := &testSession{box: struct {
		core.Mailbox
		core.MailMessageSoftDeleter
	}{box, box}}
	h := New()
	scope := placement.Identity().Scope
	items := el(messages, "ItemIds", "", at(el(types, "ItemId", ""), "Id", h.itemID(scope, item)))
	target := el(messages, "ToFolderId", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, "target")))
	for _, name := range []string{"MoveItem", "CopyItem"} {
		// DeleteType is irrelevant to MoveItem and CopyItem.
		op := at(el(messages, name, "", target, items), "DeleteType", "SoftDelete")
		serveWriteTest(t, h, softOnly, op, "ErrorInvalidPropertyForOperation")
	}
	trash := at(el(messages, "DeleteItem", "", items), "DeleteType", "MoveToDeletedItems")
	serveWriteTest(t, h, softOnly, trash, "ErrorInvalidPropertyForOperation")
	soft := at(el(messages, "DeleteItem", "", items), "DeleteType", "SoftDelete")
	serveWriteTest(t, h, placement, soft, "ErrorCannotDeleteObject")
	if box.writes != 0 {
		t.Fatal("operation without its capability performed a write")
	}
	serveWriteTest(t, h, placement, el(messages, "CopyItem", "", target, items), "NoError")
	serveWriteTest(t, h, softOnly, soft, "NoError")
	if box.writes != 2 {
		t.Fatalf("supported operations performed %d writes, want 2", box.writes)
	}
}

func TestFolderPlacementWire(t *testing.T) {
	folders := map[core.ID]core.Object{
		"root":  {ID: "root", Kind: core.Folder},
		"trash": {ID: "trash", Kind: core.Folder, Properties: core.Properties{core.WellKnown: core.Text("deleteditems")}},
		"other": {ID: "other", Kind: core.Folder},
	}
	box := &placementFixture{fixtureMailbox: fixtureMailbox{objects: folders}}
	s := &testSession{box: box}
	h := New()
	scope := s.Identity().Scope
	ref := func(id core.ID) node { return at(el(types, "FolderId", ""), "Id", h.encode(scope, id)) }
	proposal := func(folderName string) node {
		return el(types, "Folder", "", el(types, "DisplayName", folderName))
	}
	create := el(messages, "CreateFolder", "",
		el(messages, "ParentFolderId", "", at(el(types, "DistinguishedFolderId", ""), "Id", "msgfolderroot")),
		el(messages, "Folders", "", proposal("Prima")))
	created := serveWriteTest(t, h, s, create, "NoError").find(types, "FolderId")
	if created == nil {
		t.Fatal("creation omitted FolderId")
	}
	rename := func(folderRef node) node {
		return el(messages, "UpdateFolder", "", el(messages, "FolderChanges", "",
			el(types, "FolderChange", "", folderRef, el(types, "Updates", "",
				el(types, "SetFolderField", "", at(el(types, "FieldURI", ""), "FieldURI", "folder:DisplayName"), proposal("Seconda"))))))
	}
	serveWriteTest(t, h, s, rename(*created), "NoError")
	serveWriteTest(t, h, s, rename(*created), "ErrorIrresolvableConflict")
	move := el(messages, "MoveFolder", "", el(messages, "ToFolderId", "", ref("other")),
		el(messages, "FolderIds", "", ref("new")))
	if moved := serveWriteTest(t, h, s, move, "NoError").find(types, "ParentFolderId"); moved == nil || moved.attr("Id") != ref("other").attr("Id") {
		t.Fatal("new parent missing from response")
	}
	remove := func(mode string, id core.ID) node {
		return at(el(messages, "DeleteFolder", "", el(messages, "FolderIds", "", ref(id))), "DeleteType", mode)
	}
	serveWriteTest(t, h, s, remove("MoveToDeletedItems", "trash"), "ErrorDeleteDistinguishedFolder")
	serveWriteTest(t, h, s, remove("SoftDelete", "new"), "ErrorCannotDeleteObject")
	serveWriteTest(t, h, s, remove("HardDelete", "new"), "ErrorCannotDeleteObject")
	serveWriteTest(t, h, s, remove("MoveToDeletedItems", "new"), "NoError")
	folder := folders["new"]
	if folder.Parent != "trash" || folder.Properties[core.DisplayName] != core.Text("Seconda") || box.writes != 4 {
		t.Fatal("wrong folder after placement actions")
	}
}
