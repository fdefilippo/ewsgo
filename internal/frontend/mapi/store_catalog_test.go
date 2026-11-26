// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

type catalogMailbox struct {
	core.Mailbox // All operations other than the declared reads panic in tests.
	page         core.Page
	err          error
	getErr       error
	calls        int
}

func (*catalogMailbox) Root() core.ID { return "opaque-root" }
func (b *catalogMailbox) Get(_ context.Context, id core.ID) (core.Object, error) {
	if b.getErr != nil {
		return core.Object{}, b.getErr
	}
	for _, object := range b.page.Objects {
		if object.ID == id {
			return object, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}
func (b *catalogMailbox) Query(_ context.Context, q core.Query) (core.Page, error) {
	b.calls++
	if !reflect.DeepEqual(q, core.Query{Kind: core.Folder, Limit: 1000}) {
		return core.Page{}, core.ErrInvalid
	}
	return b.page, b.err
}

func catalogFixture() *catalogMailbox {
	objects := []core.Object{{ID: "opaque-root", Kind: core.Folder}}
	for _, v := range []struct{ id, role string }{{"any/not/zimbra/inbox", "inbox"}, {"opaque-sent", "sentitems"}, {"opaque-trash", "deleteditems"}} {
		objects = append(objects, core.Object{ID: core.ID(v.id), Kind: core.Folder, Parent: "opaque-root", Properties: core.Properties{core.WellKnown: core.Text(v.role), core.DisplayName: core.Text("arbitrary locale/name")}})
	}
	return &catalogMailbox{page: core.Page{Objects: objects}}
}

func TestPrivateFolderCatalogOpaqueNativeRolesAndScope(t *testing.T) {
	box := catalogFixture()
	session := &testSession{box}
	catalog, err := privateFolders(context.Background(), session)
	if err != nil || box.calls != 1 || catalog.scope != session.Identity().Scope {
		t.Fatal("catalog or scope", err)
	}
	native := map[SpecialFolderRole]core.ID{IPMSubtree: "opaque-root", Inbox: "any/not/zimbra/inbox", SentItems: "opaque-sent", DeletedItems: "opaque-trash"}
	service := 0
	for i, entry := range catalog.folders {
		if entry.Role != SpecialFolderRole(i) || entry.NativeID != native[entry.Role] {
			t.Fatal("role order or opaque native mapping", i)
		}
		if entry.NativeID == "" {
			service++
		}
	}
	if service != 9 {
		t.Fatal("gateway service roles", service)
	}
	// Returned plans are values, not aliases to mutable shared state.
	other := scopedSession{Session: session, identity: core.Identity{Scope: core.Scope{Backend: "another", Account: "different"}}}
	second, err := privateFolders(context.Background(), other)
	if err != nil || second.scope == catalog.scope {
		t.Fatal("scope isolation", err)
	}
	second.folders[Inbox].NativeID = "changed"
	if catalog.folders[Inbox].NativeID != native[Inbox] {
		t.Fatal("catalog shared between accounts")
	}
	// A renamed well-known native folder retains its semantic role. A parent
	// folder without a well-known marker can sit between root and Inbox.
	box.page.Objects = append(box.page.Objects, core.Object{ID: "nested", Kind: core.Folder, Parent: box.Root()})
	box.page.Objects[1].Parent = "nested"
	box.page.Objects[1].Properties[core.DisplayName] = core.Text("renamed")
	updated, err := privateFolders(context.Background(), session)
	if err != nil || updated.folders != catalog.folders {
		t.Fatal("native names/paths or direct-parent assumption leaked into role mapping", err)
	}
}

func TestPrivateFolderCatalogFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*catalogMailbox)
		want   error
	}{
		{"query denied", func(b *catalogMailbox) { b.err = core.ErrAccessDenied }, core.ErrAccessDenied},
		{"query canceled", func(b *catalogMailbox) { b.err = context.Canceled }, context.Canceled},
		{"truncated", func(b *catalogMailbox) { b.page.More = true }, core.ErrUnsupported},
		{"oversized", func(b *catalogMailbox) { b.page.Objects = make([]core.Object, 1001) }, core.ErrUnsupported},
		{"no root", func(b *catalogMailbox) { b.page.Objects = b.page.Objects[1:] }, core.ErrNotFound},
		{"missing role", func(b *catalogMailbox) { delete(b.page.Objects[1].Properties, core.WellKnown) }, core.ErrNotFound},
		{"wrong role type", func(b *catalogMailbox) { b.page.Objects[1].Properties[core.WellKnown] = core.Binary("inbox") }, core.ErrNotFound},
		{"duplicate role", func(b *catalogMailbox) { b.page.Objects[2].Properties[core.WellKnown] = core.Text("inbox") }, core.ErrConflict},
		{"duplicate id", func(b *catalogMailbox) { b.page.Objects[2].ID = b.page.Objects[1].ID }, core.ErrConflict},
		{"empty id", func(b *catalogMailbox) { b.page.Objects[1].ID = "" }, core.ErrInvalid},
		{"message", func(b *catalogMailbox) { b.page.Objects[1].Kind = core.Message }, core.ErrInvalid},
		{"dangling parent", func(b *catalogMailbox) { b.page.Objects[1].Parent = "missing" }, core.ErrInvalid},
		{"self cycle", func(b *catalogMailbox) { b.page.Objects[1].Parent = b.page.Objects[1].ID }, core.ErrInvalid},
		{"indirect cycle", func(b *catalogMailbox) {
			b.page.Objects[1].Parent = b.page.Objects[2].ID
			b.page.Objects[2].Parent = b.page.Objects[1].ID
		}, core.ErrInvalid},
		{"root is inbox", func(b *catalogMailbox) {
			b.page.Objects[0].Properties = core.Properties{core.WellKnown: core.Text("inbox")}
		}, core.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := catalogFixture()
			tc.change(box)
			catalog, err := privateFolders(context.Background(), &testSession{box})
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(catalog, privateFolderCatalog{}) {
				t.Fatal("invalid source yielded a partial usable catalog", err)
			}
		})
	}
	for _, session := range []core.Session{nil, &testSession{}, scopedSession{Session: &testSession{catalogFixture()}}} {
		if _, err := privateFolders(context.Background(), session); err != core.ErrInvalid {
			t.Fatal("invalid session accepted", err)
		}
	}
}
