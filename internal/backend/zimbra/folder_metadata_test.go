// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestFolderMetadataOptionalTypedAndSeconds(t *testing.T) {
	for _, attrs := range []string{``, ` ms="0" md="0"`, ` ms="73" md="1700000000"`, ` ms="9223372036854775807" md="253402300799"`} {
		var n node
		if xml.Unmarshal([]byte(`<folder id="2" l="1" name="private" view="message"`+attrs+`/>`), &n) != nil {
			t.Fatal("fixture")
		}
		f, err := folderFromNode(n)
		if err != nil {
			t.Fatal("optional valid metadata")
		}
		o := folderObject(f)
		if attrs == "" {
			if f.MetadataRevision != nil || f.ModifiedAt != nil || o.Revision != 0 || o.Properties[core.MetadataRevision] != nil || o.Properties[core.LastModifiedAt] != nil {
				t.Fatal("absence fabricated")
			}
			continue
		}
		if f.MetadataRevision == nil || f.ModifiedAt == nil || o.Revision != core.Revision(*f.MetadataRevision) || o.Properties[core.MetadataRevision] != core.Integer(*f.MetadataRevision) || o.Properties[core.LastModifiedAt] != core.Timestamp(*f.ModifiedAt) || f.ModifiedAt.Location() != time.UTC {
			t.Fatal("presence/type mapping")
		}
		if n.get("md") == "1700000000" && !f.ModifiedAt.Equal(time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)) {
			t.Fatal("md not interpreted as seconds")
		}
	}
}

func TestFolderMetadataInvalidHasNoPartialResult(t *testing.T) {
	for _, attrs := range []string{` ms=""`, ` md=""`, ` ms="-1"`, ` md="-1"`, ` ms="private"`, ` md="1.5"`, ` ms="9223372036854775808"`, ` md="253402300800"`, ` md="1700000000000"`, ` ms="1" ms="2"`} {
		var n node
		if xml.Unmarshal([]byte(`<folder id="2"`+attrs+`/>`), &n) != nil {
			t.Fatal("fixture")
		}
		f, err := folderFromNode(n)
		if !errors.Is(err, ErrProtocol) || !reflect.DeepEqual(f, Folder{}) {
			t.Fatal("invalid metadata retained partial folder")
		}
	}
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" ms="7"/><folder id="3" md="bad"/></folder></GetFolderResponse>`)
	}, Config{})
	session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}
	if folders, err := session.Folders(context.Background(), ""); !errors.Is(err, ErrProtocol) || folders != nil {
		t.Fatal("partially parsed catalog escaped")
	}
}

func TestFolderMetadataQueryGetPreserveMailViewFilter(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" ms="1"><folder id="2" l="1" view="message" ms="7" md="1700000000"/><folder id="7" l="1" view="appointment" ms="8" md="1700000001"/></folder></GetFolderResponse>`)
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "synthetic"}}
	page, err := box.Query(context.Background(), core.Query{Kind: core.Folder, Limit: 1000})
	if err != nil || len(page.Objects) != 2 || page.More {
		t.Fatal("current view scope changed")
	}
	get, err := box.Get(context.Background(), "f:2")
	if err != nil || !reflect.DeepEqual(get, page.Objects[1]) || get.Revision != 7 || get.Properties[core.LastModifiedAt] != core.Timestamp(time.Unix(1700000000, 0).UTC()) {
		t.Fatal("Get/Query metadata contract")
	}
	if _, err := box.Get(context.Background(), "f:7"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("non-mail view access expanded")
	}
	if box.Capabilities().Write {
		t.Fatal("metadata added write capability")
	}
}

func TestFolderCountersOptionalValidatedAndMapped(t *testing.T) {
	for _, tc := range []struct {
		attrs         string
		count, unread *uint64
	}{
		{attrs: ""},
		{attrs: ` n="0" u="0"`, count: func() *uint64 { v := uint64(0); return &v }(), unread: func() *uint64 { v := uint64(0); return &v }()},
		{attrs: ` n="3" u="1"`, count: func() *uint64 { v := uint64(3); return &v }(), unread: func() *uint64 { v := uint64(1); return &v }()},
		{attrs: ` n="2147483647"`, count: func() *uint64 { v := uint64(2147483647); return &v }()},
		{attrs: ` u="2"`, unread: func() *uint64 { v := uint64(2); return &v }()},
	} {
		var n node
		if xml.Unmarshal([]byte(`<folder id="2"`+tc.attrs+`/>`), &n) != nil {
			t.Fatal("fixture")
		}
		f, err := folderFromNode(n)
		if err != nil || !reflect.DeepEqual(f.ItemCount, tc.count) || !reflect.DeepEqual(f.UnreadCount, tc.unread) {
			t.Fatal("optional count presence/value")
		}
		o := folderObject(f)
		for key, number := range map[core.Key]*uint64{core.FolderItemCount: tc.count, core.FolderUnreadCount: tc.unread} {
			if number == nil && o.Properties[key] != nil || number != nil && o.Properties[key] != core.Integer(*number) {
				t.Fatal("semantic count mapping")
			}
		}
	}
	for _, attrs := range []string{` n=""`, ` u="-1"`, ` n="1.5"`, ` n="2147483648"`, ` n="2" u="3"`, ` n="2" n="3"`, ` u="1" u="1"`} {
		var n node
		if xml.Unmarshal([]byte(`<folder id="2"`+attrs+`/>`), &n) != nil {
			t.Fatal("fixture")
		}
		if f, err := folderFromNode(n); !errors.Is(err, ErrProtocol) || !reflect.DeepEqual(f, Folder{}) {
			t.Fatal("invalid counters leaked partial folder")
		}
	}
}

func TestFullFolderCatalogOmittedUnreadIsZeroOnlyForCountedNodes(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" n="3"/><folder id="3" n="2" u="1"/></folder></GetFolderResponse>`)
	}, Config{})
	session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}
	folders, err := session.Folders(context.Background(), "")
	if err != nil || len(folders) != 3 {
		t.Fatal("full catalog fixture")
	}
	if folders[0].ItemCount != nil || folders[0].UnreadCount != nil {
		t.Fatal("structural placeholder counts fabricated")
	}
	if folders[1].UnreadCount == nil || *folders[1].UnreadCount != 0 || folders[2].UnreadCount == nil || *folders[2].UnreadCount != 1 {
		t.Fatal("full GetFolder zero-omission contract")
	}
}
