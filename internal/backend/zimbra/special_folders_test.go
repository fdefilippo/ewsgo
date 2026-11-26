// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"ewsgo/internal/core"
	"net/http"
	"testing"
	"time"
)

func TestOwnFolderTreeReadsAllRequestedMetadataOnce(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request node
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("decode folder request:", err)
			return
		}
		folders := request.child(soapNS, "Body").child(mailNS, "GetFolderRequest")
		if folders == nil || folders.get("tr") != "0" || folders.get("depth") != "-1" ||
			folders.child(mailNS, "folder").get("path") != "/" ||
			request.child(soapNS, "Header").child("urn:zimbra", "context").child("urn:zimbra", "account") != nil {
			t.Error("expected own mailbox root with mount traversal disabled")
		}
		calls++
		cal := `<folder id="10" l="1" view="appointment" ms="7"/>`
		if calls == 2 {
			cal = ""
		}
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1">`+cal+
			`<folder id="7" l="1" view="contact"/><folder id="15" l="1" view="task"/>`+
			`<folder id="20" l="1" view="message"/><folder id="25" l="1" view="appointment"/>`+
			`<link id="99"><folder id="30" l="1" view="message"/></link></folder></GetFolderResponse>`)
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
	roles := []core.SpecialFolderKind{core.CalendarFolder, core.ContactsFolder, core.TasksFolder}
	ctx := context.Background()
	for read := 1; read <= 2; read++ {
		objects, err := box.OwnFolderTree(ctx, roles)
		if err != nil || calls != read || len(objects) != 6-read {
			t.Fatalf("read %d: %d calls, %d folders, error %v", read, calls, len(objects), err)
		}
		want := map[core.ID]core.Value{"f:1": core.Text("msgfolderroot"), "f:20": nil, "f:7": core.Text("contacts"), "f:15": core.Text("tasks")}
		if read == 1 {
			want["f:10"] = core.Text("calendar")
		}
		for _, object := range objects {
			if role, ok := want[object.ID]; !ok || object.Properties[core.WellKnown] != role {
				t.Fatalf("unexpected folder or role for %s", object.ID)
			}
			delete(want, object.ID)
		}
		if len(want) != 0 {
			t.Fatalf("missing %d own folders", len(want))
		}
	}
}

func TestSpecialCalendarOwnMetadataAndMailIsolation(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var e node
		if xml.NewDecoder(r.Body).Decode(&e) != nil {
			t.Fatal("request")
		}
		req := e.child(soapNS, "Body").child(mailNS, "GetFolderRequest")
		if req == nil {
			t.Fatal("operation beyond metadata")
		}
		calls++
		if calls == 1 && e.child(soapNS, "Header").child("urn:zimbra", "context").child("urn:zimbra", "account") != nil {
			t.Fatal("external account selector")
		}
		if calls == 1 && req.get("tr") != "0" {
			t.Fatal("mount traversal not disabled")
		}
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" l="1" view="message"/><folder id="10" l="1" name="Calendario locale" view="appointment" ms="7"/><link id="99"><folder id="10" l="99" view="appointment"/></link></folder></GetFolderResponse>`)
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "synthetic"}}
	o, err := box.SpecialFolder(context.Background(), core.CalendarFolder)
	if err != nil || o.ID != "f:10" || o.Parent != "f:1" || o.Revision != 7 || o.Properties[core.WellKnown] != core.Text("calendar") || o.Properties[core.DisplayName] != core.Text("Calendario locale") {
		t.Fatal("native role metadata", err)
	}
	if _, err := box.Get(context.Background(), o.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail-only Get expanded")
	}
	page, err := box.Query(context.Background(), core.Query{Kind: core.Folder, Limit: 1000})
	if err != nil || len(page.Objects) != 2 || box.Capabilities().Write {
		t.Fatal("mail-only Query or capabilities changed")
	}
	if _, err := box.SpecialFolder(context.Background(), 99); !errors.Is(err, core.ErrUnsupported) || calls != 3 {
		t.Fatal("unknown role caused read")
	}
}

func TestSpecialCalendarRejectsInvalidNativeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, xml string
		want      error
	}{
		{"absent", `<folder id="1"/>`, core.ErrNotFound},
		{"wrong view", `<folder id="1"><folder id="10" l="1" view="message"/></folder>`, ErrProtocol},
		{"duplicate", `<folder id="1"><folder id="10" l="1" view="appointment"/><folder id="10" l="1" view="appointment"/></folder>`, core.ErrConflict},
		{"dangling", `<folder id="1"><folder id="10" l="99" view="appointment"/></folder>`, ErrProtocol},
		{"cycle", `<folder id="1"><folder id="10" l="11" view="appointment"/><folder id="11" l="10"/></folder>`, ErrProtocol},
		{"foreign id", `<folder id="1"><folder id="foreign:10" l="1" view="appointment"/></folder>`, ErrProtocol},
		{"mount only", `<folder id="1"><link id="99"><folder id="10" l="1" view="appointment"/></link></folder>`, core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail">`+tc.xml+`</GetFolderResponse>`)
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
			if _, err := box.SpecialFolder(context.Background(), core.CalendarFolder); !errors.Is(err, tc.want) {
				t.Fatal("invalid catalog escaped", err)
			}
		})
	}
}

func TestSpecialContactsOwnMetadataAndMailIsolation(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var e node
		if xml.NewDecoder(r.Body).Decode(&e) != nil {
			t.Fatal("request")
		}
		req := e.child(soapNS, "Body").child(mailNS, "GetFolderRequest")
		if req == nil {
			t.Fatal("operation beyond metadata")
		}
		calls++
		if calls == 1 && e.child(soapNS, "Header").child("urn:zimbra", "context").child("urn:zimbra", "account") != nil {
			t.Fatal("external account selector")
		}
		if calls == 1 && req.get("tr") != "0" {
			t.Fatal("mount traversal not disabled")
		}
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" l="1" view="message"/><folder id="7" l="1" name="Contatti locali" view="contact" ms="7"/><link id="99"><folder id="7" l="99" view="contact"/></link></folder></GetFolderResponse>`)
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "synthetic"}}
	o, err := box.SpecialFolder(context.Background(), core.ContactsFolder)
	if err != nil || o.ID != "f:7" || o.Parent != "f:1" || o.Revision != 7 || o.Properties[core.WellKnown] != core.Text("contacts") || o.Properties[core.DisplayName] != core.Text("Contatti locali") {
		t.Fatal("native role metadata", err)
	}
	if _, err := box.Get(context.Background(), o.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail-only Get expanded")
	}
	page, err := box.Query(context.Background(), core.Query{Kind: core.Folder, Limit: 1000})
	if err != nil || len(page.Objects) != 2 || box.Capabilities().Write {
		t.Fatal("mail-only Query or capabilities changed")
	}
	if _, err := box.SpecialFolder(context.Background(), 99); !errors.Is(err, core.ErrUnsupported) || calls != 3 {
		t.Fatal("unknown role caused read")
	}
}

func TestSpecialContactsRejectsInvalidNativeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, xml string
		want      error
	}{
		{"absent", `<folder id="1"/>`, core.ErrNotFound},
		{"wrong view", `<folder id="1"><folder id="7" l="1" view="message"/></folder>`, ErrProtocol},
		{"duplicate", `<folder id="1"><folder id="7" l="1" view="contact"/><folder id="7" l="1" view="contact"/></folder>`, core.ErrConflict},
		{"dangling", `<folder id="1"><folder id="7" l="99" view="contact"/></folder>`, ErrProtocol},
		{"cycle", `<folder id="1"><folder id="7" l="11" view="contact"/><folder id="11" l="7"/></folder>`, ErrProtocol},
		{"foreign id", `<folder id="1"><folder id="foreign:7" l="1" view="contact"/></folder>`, ErrProtocol},
		{"mount only", `<folder id="1"><link id="99"><folder id="7" l="1" view="contact"/></link></folder>`, core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail">`+tc.xml+`</GetFolderResponse>`)
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
			if _, err := box.SpecialFolder(context.Background(), core.ContactsFolder); !errors.Is(err, tc.want) {
				t.Fatal("invalid catalog escaped", err)
			}
		})
	}
}

func TestSpecialTasksOwnMetadataAndMailIsolation(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var e node
		if xml.NewDecoder(r.Body).Decode(&e) != nil {
			t.Fatal("request")
		}
		req := e.child(soapNS, "Body").child(mailNS, "GetFolderRequest")
		if req == nil {
			t.Fatal("operation beyond metadata")
		}
		calls++
		if calls == 1 && e.child(soapNS, "Header").child("urn:zimbra", "context").child("urn:zimbra", "account") != nil {
			t.Fatal("external account selector")
		}
		if calls == 1 && req.get("tr") != "0" {
			t.Fatal("mount traversal not disabled")
		}
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" l="1" view="message"/><folder id="15" l="1" name="Attivita locali" view="task" ms="7"/><link id="99"><folder id="15" l="99" view="task"/></link></folder></GetFolderResponse>`)
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "synthetic"}}
	o, err := box.SpecialFolder(context.Background(), core.TasksFolder)
	if err != nil || o.ID != "f:15" || o.Parent != "f:1" || o.Revision != 7 || o.Properties[core.WellKnown] != core.Text("tasks") || o.Properties[core.DisplayName] != core.Text("Attivita locali") {
		t.Fatal("native role metadata", err)
	}
	if _, err := box.Get(context.Background(), o.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail-only Get expanded")
	}
	page, err := box.Query(context.Background(), core.Query{Kind: core.Folder, Limit: 1000})
	if err != nil || len(page.Objects) != 2 || box.Capabilities().Write {
		t.Fatal("mail-only Query or capabilities changed")
	}
	if _, err := box.SpecialFolder(context.Background(), 99); !errors.Is(err, core.ErrUnsupported) || calls != 3 {
		t.Fatal("unknown role caused read")
	}
}

func TestSpecialTasksRejectsInvalidNativeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, xml string
		want      error
	}{
		{"absent", `<folder id="1"/>`, core.ErrNotFound},
		{"wrong view", `<folder id="1"><folder id="15" l="1" view="message"/></folder>`, ErrProtocol},
		{"duplicate", `<folder id="1"><folder id="15" l="1" view="task"/><folder id="15" l="1" view="task"/></folder>`, core.ErrConflict},
		{"dangling", `<folder id="1"><folder id="15" l="99" view="task"/></folder>`, ErrProtocol},
		{"cycle", `<folder id="1"><folder id="15" l="11" view="task"/><folder id="11" l="15"/></folder>`, ErrProtocol},
		{"foreign id", `<folder id="1"><folder id="foreign:15" l="1" view="task"/></folder>`, ErrProtocol},
		{"mount only", `<folder id="1"><link id="99"><folder id="15" l="1" view="task"/></link></folder>`, core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail">`+tc.xml+`</GetFolderResponse>`)
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
			if _, err := box.SpecialFolder(context.Background(), core.TasksFolder); !errors.Is(err, tc.want) {
				t.Fatal("invalid catalog escaped", err)
			}
		})
	}
}
