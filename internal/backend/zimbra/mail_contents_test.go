// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type contentFixture struct {
	folders, index, metadata, mime, emails string
	search, gets, modified, bodies         int
	before                                 func(*contentFixture, http.ResponseWriter, *http.Request, string) bool
}

func newContentFixture() *contentFixture {
	return &contentFixture{
		folders:  `<folder id="1"><folder id="2" l="1" view="message"/><folder id="10" l="1" view="appointment"/><link id="90"><folder id="91" l="90"/></link></folder>`,
		index:    `<m id="91" l="2"/><m id="92" l="2"/>`,
		metadata: `l="2" ms="7" rev="3" d="1794736800000" s="120" f="u"`, mime: fixtureMIME,
	}
}
func (f *contentFixture) box(t *testing.T) *mailbox {
	t.Helper()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			cookie, e := r.Cookie("ZM_AUTH_TOKEN")
			if e != nil || cookie.Value != fixtureToken {
				t.Error("missing native cookie")
			}
			if !nativeID.MatchString(r.URL.Query().Get("id")) {
				t.Error("bad native selector")
			}
			switch r.URL.Path {
			case "/service/content/get":
				if r.Method != http.MethodHead {
					t.Error("expected HEAD for message metadata")
				}
				f.modified++
				if f.before != nil && f.before(f, w, r, "modified") {
					return
				}
				w.Header().Set("X-Zimbra-Modified", "1794736800123")
				w.Header().Set("X-Zimbra-Flags", "u")
				fmt.Fprint(w, "X-Zimbra-Modified: injected inline metadata\n"+f.mime)
			case "/service/home/~/":
				f.bodies++
				if f.before != nil && f.before(f, w, r, "mime") {
					return
				}
				if r.URL.Query().Get("fmt") != "raw" || r.URL.Query().Get("auth") != "co" {
					t.Error("unexpected MIME request")
				}
				fmt.Fprint(w, f.mime)
			default:
				t.Error("unexpected HTTP operation")
				w.WriteHeader(500)
			}
			return
		}
		var e node
		if xml.NewDecoder(r.Body).Decode(&e) != nil {
			t.Error("bad SOAP")
			return
		}
		context := e.child(soapNS, "Header").child("urn:zimbra", "context")
		if context == nil || context.child("urn:zimbra", "account") != nil {
			t.Error("foreign account selector")
		}
		b := e.child(soapNS, "Body")
		if b == nil || len(b.Children) != 1 {
			t.Error("bad SOAP body")
			return
		}
		req := b.Children[0]
		switch req.XMLName.Local {
		case "GetFolderRequest":
			if req.get("tr") != "0" {
				t.Error("mount traversal enabled")
			}
			if f.before != nil && f.before(f, w, r, "folders") {
				return
			}
			envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail">`+f.folders+`</GetFolderResponse>`)
		case "SearchRequest":
			f.search++
			if req.get("types") != "message" || req.get("limit") != "257" || req.get("offset") != "0" || req.child(mailNS, "query").Text != "inid:2" {
				t.Error("unbounded or non-mail/exact search")
			}
			if f.before != nil && f.before(f, w, r, "index") {
				return
			}
			envelope(w, `<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0">`+f.index+`</SearchResponse>`)
		case "GetMsgRequest":
			f.gets++
			m := req.child(mailNS, "m")
			if m == nil || m.get("read") != "0" || m.get("wantModSeq") != "1" {
				t.Error("metadata request may mutate or lacks revision")
			}
			if !strings.Contains(f.index, `id="`+m.get("id")+`"`) {
				t.Error("unlisted message read")
			}
			if f.before != nil && f.before(f, w, r, "message") {
				return
			}
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="`+m.get("id")+`" `+f.metadata+`><su>Complete synthetic subject</su><fr>Preview</fr>`+f.emails+`</m></GetMsgResponse>`)
		default:
			t.Error("unexpected native operation; possible write")
			w.WriteHeader(500)
		}
	}, Config{})
	return &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "synthetic"}}
}
func TestMailContentsCompleteNativeDateAndMIME(t *testing.T) {
	f := newContentFixture()
	box := f.box(t)
	got, err := box.ReadMailContents(context.Background(), "f:2")
	if err != nil || got.Folder != "f:2" || len(got.Messages) != 2 {
		t.Fatal("complete view unavailable", err)
	}
	for _, o := range got.Messages {
		if o.Parent != "f:2" || o.Kind != core.Message || o.Revision != 7 || o.Properties[core.MetadataRevision] != core.Integer(7) || o.Properties[core.ContentRevision] != core.Integer(3) || o.Properties[core.LastModifiedAt] != core.Timestamp(time.UnixMilli(1794736800123).UTC()) || o.Properties[core.IsRead] != core.Bool(false) || string(o.MIME) != fixtureMIME {
			t.Fatal("native date/version/body not preserved")
		}
	}
	if f.search != 2 || f.gets != 6 || f.bodies != 2 || f.modified != 2 {
		t.Fatal("missing membership/version brackets")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", got, got), "synthetic subject") {
		t.Fatal("view formatting exposed values")
	}
	// Caller-owned output never aliases a server-side cache or later result.
	got.Messages[0].MIME[0] = 'X'
	again, err := box.ReadMailContents(context.Background(), "f:2")
	if err != nil || string(again.Messages[0].MIME) != fixtureMIME {
		t.Fatal("mutable response shared across captures")
	}
}

func TestMailContentsProjectsNativeCategoryNames(t *testing.T) {
	f := newContentFixture()
	f.metadata += ` tn="Blue,Later\, Today,Café"`
	got, err := f.box(t).ReadMailContents(t.Context(), "f:2")
	want := core.Strings{"Blue", "Later, Today", "Café"}
	if err != nil || len(got.Messages) != 2 {
		t.Fatal("category view unavailable", err)
	}
	for _, message := range got.Messages {
		tags, ok := message.Properties[core.MailCategories].(core.Strings)
		if !ok || !reflect.DeepEqual(tags, want) {
			t.Fatal("native tag names did not survive the contents snapshot")
		}
	}
}

func TestMailContentsRecipientRolesNamesAndOwnership(t *testing.T) {
	f := newContentFixture()
	f.emails = `<e t="f" a="sender@example.invalid"/><e t="t" a="same@example.invalid" p="Full Personal Name" d="Full"/><e t="c" a="same@example.invalid" d="Short Native"/><e t="b" a="blind@example.invalid"/><e t="s"/><e t="r"/><e t="rf"/><e t="n"/>`
	box := f.box(t)
	got, err := box.ReadMailContents(context.Background(), "f:2")
	want := []core.Recipient{{Kind: core.RecipientTo, Address: "same@example.invalid", DisplayName: "Full Personal Name"}, {Kind: core.RecipientCc, Address: "same@example.invalid", DisplayName: "Short Native"}, {Kind: core.RecipientBcc, Address: "blind@example.invalid"}}
	if err != nil || len(got.Messages) != 2 || !reflect.DeepEqual(got.Messages[0].Recipients, want) {
		t.Fatal("native roles/names not preserved", err)
	}
	got.Messages[0].Recipients[0].Kind = core.RecipientBcc
	got.Messages[0].Recipients[0].DisplayName = "caller edit"
	again, err := box.ReadMailContents(context.Background(), "f:2")
	if err != nil || !reflect.DeepEqual(again.Messages[0].Recipients, want) || !reflect.DeepEqual(got.Messages[1].Recipients, want) {
		t.Fatal("recipient output aliased another capture/message")
	}
}

func TestMailContentsRecipientsRejectMalformedUnsupportedOrOverBudget(t *testing.T) {
	for _, payload := range []string{
		`<e t="t" a="invalid"/>`, `<e t="t" a="Name &lt;valid@example.invalid&gt;"/>`,
		`<e t="t" a="valid@example.invalid" p="bad&#10;name"/>`,
		`<e t="unknown" a="valid@example.invalid"/>`, `<e a="valid@example.invalid"/>`,
		`<e t="t" a="group@example.invalid" isGroup="1"/>`,
		`<e t="c" a="group@example.invalid" exp="true"/>`,
		`<e t="b" a="valid@example.invalid" isGroup="invalid"/>`,
		`<e xmlns="foreign" t="t" a="valid@example.invalid"/>`,
		`<e t="t" a="valid@example.invalid"><e/></e>`,
		`<e t="t" t="c" a="valid@example.invalid"/>`,
		`<e t="t" a="valid@example.invalid" a="other@example.invalid"/>`,
		`<e t="t" xmlns:x="foreign" x:a="valid@example.invalid"/>`,
		strings.Repeat(`<e t="t" a="valid@example.invalid"/>`, 257),
		`<e t="t" a="valid@example.invalid" p="` + strings.Repeat("x", 64<<10) + `"/>`,
	} {
		f := newContentFixture()
		f.emails = payload
		got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
		if err == nil || !reflect.DeepEqual(got, core.MailContents{}) || f.bodies != 0 {
			t.Fatal("invalid recipients yielded usable contents")
		}
	}
}

func TestMailContentsRecipientRoleChurnRejectsWholeView(t *testing.T) {
	f := newContentFixture()
	f.index = `<m id="91" l="2"/>`
	f.before = func(f *contentFixture, _ http.ResponseWriter, _ *http.Request, op string) bool {
		if op == "message" {
			role := "t"
			if f.gets%2 == 0 {
				role = "c"
			}
			f.emails = `<e t="` + role + `" a="synthetic@example.invalid"/>`
		}
		return false
	}
	got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
	if !errors.Is(err, core.ErrConflict) || !reflect.DeepEqual(got, core.MailContents{}) || f.bodies != 3 {
		t.Fatal("recipient-only churn bypassed bounded consistency checks", err)
	}
}
func TestMailContentsEmptyAndNonmail(t *testing.T) {
	f := newContentFixture()
	f.index = ""
	box := f.box(t)
	got, err := box.ReadMailContents(context.Background(), "f:2")
	if err != nil || got.Folder != "f:2" || len(got.Messages) != 0 || f.search != 2 || f.gets != 0 || f.bodies != 0 {
		t.Fatal("genuine empty not verified", err)
	}
	for _, tc := range []struct {
		id   core.ID
		want error
	}{{"f:10", core.ErrUnsupported}, {"f:99", core.ErrNotFound}, {"f:91", core.ErrNotFound}, {"f:other:2", core.ErrInvalid}, {"m:2", core.ErrInvalid}} {
		if _, err := box.ReadMailContents(context.Background(), tc.id); !errors.Is(err, tc.want) {
			t.Fatal("invalid or nonmail target accepted", tc.id, err)
		}
	}
	if f.search != 2 || f.modified != 0 {
		t.Fatal("Calendar or nonnative target queried")
	}
}
func TestMailContentsIndexRejectsPartialOrAmbiguous(t *testing.T) {
	for _, payload := range []string{
		`<SearchResponse xmlns="urn:zimbraMail" more="1" offset="0"><m id="91" l="2"/></SearchResponse>`,
		`<SearchResponse xmlns="urn:zimbraMail" offset="0"/>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0"/>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0"><m id="91" l="2"/><m id="91" l="2"/></SearchResponse>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0"><m id="other:91" l="2"/></SearchResponse>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0"><m id="91" l="3"/></SearchResponse>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0"><appt id="91" l="2"/></SearchResponse>`,
		`<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0"><m xmlns="foreign" id="91" l="2"/></SearchResponse>`,
	} {
		t.Run(fmt.Sprint(len(payload), payload[0:15]), func(t *testing.T) {
			f := newContentFixture()
			f.before = func(_ *contentFixture, w http.ResponseWriter, _ *http.Request, op string) bool {
				if op == "index" {
					envelope(w, payload)
					return true
				}
				return false
			}
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if err == nil || !reflect.DeepEqual(got, core.MailContents{}) || f.gets != 0 || f.bodies != 0 {
				t.Fatal("partial index produced usable view")
			}
		})
	}
}
func TestMailContentsMetadataStrictPresenceAndNoFallback(t *testing.T) {
	for _, attribute := range []string{"ms", "rev", "s", "d"} {
		for _, replace := range []string{"", attribute + `="not-a-number"`, attribute + `="-1"`} {
			t.Run(attribute+replace, func(t *testing.T) {
				f := newContentFixture()
				f.index = `<m id="91" l="2"/>`
				parts := strings.Fields(f.metadata)
				for i, s := range parts {
					if strings.HasPrefix(s, attribute+"=") {
						parts[i] = replace
					}
				}
				f.metadata = strings.Join(parts, " ")
				got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
				if err == nil || len(got.Messages) != 0 || f.bodies != 0 {
					t.Fatal("missing/malformed metadata assumed valid")
				}
			})
		}
	}
	for _, meta := range []string{`md="invalid"`, `md="253402300800"`, `ms="8"`, `md="1794736801"`} {
		t.Run(meta, func(t *testing.T) {
			f := newContentFixture()
			f.metadata += " " + meta
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if err == nil || len(got.Messages) != 0 {
				t.Fatal("invalid/duplicate/inconsistent metadata accepted")
			}
		})
	}
	f := newContentFixture()
	f.before = func(_ *contentFixture, w http.ResponseWriter, _ *http.Request, op string) bool {
		if op == "modified" {
			w.WriteHeader(200)
			return true
		}
		return false
	}
	got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
	if err == nil || len(got.Messages) != 0 {
		t.Fatal("missing native date became fallback")
	}
}
func TestMailContentsBoundedRetriesAndNoPartialOnChurn(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			f := newContentFixture()
			f.index = `<m id="91" l="2"/>`
			f.before = func(f *contentFixture, _ http.ResponseWriter, _ *http.Request, op string) bool {
				if op == "message" {
					version := f.gets
					if stable && version > 1 {
						version = 2
					}
					f.metadata = fmt.Sprintf(`l="2" ms="%d" rev="3" d="1794736800000" s="120" f="u"`, version)
				}
				return false
			}
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if stable {
				if err != nil || len(got.Messages) != 1 || f.bodies != 2 {
					t.Fatal("bounded retry did not recover", err)
				}
			} else {
				if !errors.Is(err, core.ErrConflict) || !reflect.DeepEqual(got, core.MailContents{}) || f.bodies != 3 {
					t.Fatal("churn produced partial or unbounded retries", err)
				}
			}
		})
	}
	f := newContentFixture()
	f.index = `<m id="91" l="2"/>`
	f.before = func(f *contentFixture, _ http.ResponseWriter, _ *http.Request, op string) bool {
		if op == "index" && f.search > 1 {
			f.index = ""
		}
		return false
	}
	got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
	if err != nil || len(got.Messages) != 0 || f.bodies != 1 || f.search != 4 {
		t.Fatal("membership race not retried atomically", err)
	}
}
func TestMailContentsLimitsAndFailuresDiscardView(t *testing.T) {
	for _, op := range []string{"mime", "modified", "final", "large_mime", "large_catalog"} {
		t.Run(op, func(t *testing.T) {
			f := newContentFixture()
			f.before = func(f *contentFixture, w http.ResponseWriter, r *http.Request, stage string) bool {
				if op == "large_mime" && stage == "mime" {
					fmt.Fprint(w, strings.Repeat("x", core.MaxMailContentMIMEBytes+1))
					return true
				}
				if op == "large_catalog" && stage == "index" {
					var b strings.Builder
					for i := 1; i <= 257; i++ {
						fmt.Fprintf(&b, `<m id="%d" l="2"/>`, i)
					}
					envelope(w, `<SearchResponse xmlns="urn:zimbraMail" more="0" offset="0">`+b.String()+`</SearchResponse>`)
					return true
				}
				if (op == stage && r.URL.Query().Get("id") == "92") || (op == "final" && stage == "message" && f.gets == 5) {
					w.WriteHeader(403)
					return true
				}
				return false
			}
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if err == nil || !reflect.DeepEqual(got, core.MailContents{}) {
				t.Fatal("failure yielded partial success")
			}
		})
	}
}
func TestMailContentsCancellationAndCaptureQuota(t *testing.T) {
	f := newContentFixture()
	box := f.box(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := box.ReadMailContents(ctx, "f:2"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	for i := 0; i < cap(mailContentCaptures); i++ {
		mailContentCaptures <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(mailContentCaptures); i++ {
			<-mailContentCaptures
		}
	}()
	if _, err := box.ReadMailContents(context.Background(), "f:2"); !errors.Is(err, ErrLimit) || f.search != 0 {
		t.Fatal("capture quota not enforced before queries")
	}
}

func TestMailContentsNativeRootParentAndCatalogValidation(t *testing.T) {
	for _, tc := range []struct {
		catalog string
		valid   bool
	}{
		{`<folder id="1" l="11"><folder id="2" l="1" view="message"/></folder>`, true},
		{`<folder id="1" view="appointment"><folder id="2" l="1"/></folder>`, false},
		{`<folder id="1"><folder id="2" l="3"/><folder id="3" l="2"/></folder>`, false},
		{`<folder id="1"><folder id="2" l="99"/></folder>`, false},
		{`<folder id="1"><folder id="2" l="1"/><folder id="2" l="1"/></folder>`, false},
		{`<folder id="1"><folder id="other:2" l="1"/></folder>`, false},
	} {
		t.Run(fmt.Sprint(tc.valid, len(tc.catalog)), func(t *testing.T) {
			f := newContentFixture()
			f.folders = tc.catalog
			f.index = ""
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if (err == nil) != tc.valid || (!tc.valid && (!reflect.DeepEqual(got, core.MailContents{}) || f.search != 0)) {
				t.Fatal("native root/ancestry validation incorrect", err)
			}
		})
	}
}
func TestMailContentsZeroMetadataPresenceAndNativeDate(t *testing.T) {
	f := newContentFixture()
	f.metadata = `l="2" ms="0" rev="0" md="1794736800" d="0" s="0" f="u"`
	got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
	if err != nil || got.Messages[0].Properties[core.MetadataRevision] != core.Integer(0) || got.Messages[0].Properties[core.ContentRevision] != core.Integer(0) {
		t.Fatal("supplied zero confused with absence", err)
	}
}
func TestMailContentsFinalVersionRecheckAndMetadataRedirect(t *testing.T) {
	for _, op := range []string{"final_change", "metadata_redirect", "metadata_duplicate_date", "aggregate", "empty_mime"} {
		t.Run(op, func(t *testing.T) {
			f := newContentFixture()
			if op == "aggregate" {
				f.index = `<m id="91" l="2"/><m id="92" l="2"/><m id="93" l="2"/><m id="94" l="2"/>`
				f.mime = strings.Repeat("a", core.MaxMailContentMIMEBytes)
			}
			f.before = func(f *contentFixture, w http.ResponseWriter, r *http.Request, stage string) bool {
				switch {
				case op == "empty_mime" && stage == "mime":
					w.WriteHeader(200)
					return true
				case op == "final_change" && stage == "message":
					f.metadata = fmt.Sprintf(`l="2" ms="%d" rev="3" d="1794736800000" s="120" f="u"`, (f.gets-1)/4+1)
				case op == "metadata_redirect" && stage == "modified":
					w.Header().Set("Location", "https://untrusted.invalid/path")
					w.WriteHeader(307)
					return true
				case op == "metadata_duplicate_date" && stage == "modified":
					w.Header().Add("X-Zimbra-Modified", "1794736800123")
					w.Header().Add("X-Zimbra-Modified", "1794736800123")
					w.Header().Set("X-Zimbra-Flags", "u")
					return true
				case op == "aggregate" && stage == "message":
					// The fixture has already decoded the request.
					ids := []string{"91", "92", "93", "94"}
					id := ids[(f.gets-1)/2]
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="`+id+`" `+f.metadata+`/></GetMsgResponse>`)
					return true
				}
				return false
			}
			got, err := f.box(t).ReadMailContents(context.Background(), "f:2")
			if err == nil || !reflect.DeepEqual(got, core.MailContents{}) {
				t.Fatal("failed capture emitted usable result")
			}
		})
	}
}

func TestMailMessageEnvelopeUsesExactParentAndDoesNotReadMIME(t *testing.T) {
	f := newContentFixture()
	f.emails = `<e t="t" a="fixture@example.invalid" p="Synthetic"/>`
	box := f.box(t)
	ctx := context.Background()
	o, err := box.ReadMailMessageEnvelope(ctx, "f:2", "m:91", false)
	if err != nil || o.ID != "m:91" || o.Parent != "f:2" || len(o.MIME) != 0 || len(o.Recipients) != 1 || o.Recipients[0].Kind != core.RecipientTo || o.Properties[core.IsRead] != core.Bool(false) {
		t.Fatal("native envelope contract failed", err)
	}
	f.metadata = strings.Replace(f.metadata, `f="u"`, `f="ux"`, 1)
	if _, err := box.ReadMailMessageEnvelope(ctx, "f:2", "m:91", false); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("soft-deleted message escaped without permission", err)
	}
	if _, err := box.ReadMailMessageEnvelope(ctx, "f:2", "m:91", true); err != nil {
		t.Fatal("soft-deleted message unavailable with flag", err)
	}
	f.metadata = strings.Replace(f.metadata, `l="2"`, `l="3"`, 1)
	if _, err := box.ReadMailMessageEnvelope(ctx, "f:2", "m:91", true); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("wrong native parent escaped lookup", err)
	}
	if _, err := box.ReadMailMessageEnvelope(ctx, "f:2", "m:91:other", false); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("qualified message selector accepted", err)
	}
	if f.gets != 4 || f.search != 0 || f.modified != 0 || f.bodies != 0 {
		t.Fatal("envelope lookup used a non-metadata operation")
	}
}

func TestMailMessageEnvelopeReportsCalendarAlternativeAsAttachment(t *testing.T) {
	f := newContentFixture()
	box := f.box(t)
	for _, tc := range []struct {
		name, parts string
		hasAttach   bool
	}{
		{"plain alternative", `<mp ct="multipart/alternative"><mp ct="text/plain"/><mp ct="text/html"/></mp>`, false},
		{"calendar alternative", `<mp ct="multipart/mixed"><mp ct="multipart/alternative"><mp ct="text/plain"/><mp ct="text/calendar"/></mp></mp>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.emails = tc.parts
			got, err := box.ReadMailMessageEnvelope(t.Context(), "f:2", "m:91", false)
			if err != nil || got.Properties[core.HasAttachments] != core.Bool(tc.hasAttach) || f.bodies != 0 {
				t.Fatal("unexpected attachment indication or MIME fetch", err)
			}
		})
	}
}

func TestMailMessageMIMEVerifiesParentAndContentRevision(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		want           error
	}{
		{name: "read-state change", from: `ms="7" rev="3"`, to: `ms="8" rev="3"`},
		{name: "content change", from: `rev="3"`, to: `rev="4"`, want: core.ErrConflict},
		{name: "message moved", from: `l="2"`, to: `l="3"`, want: core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContentFixture()
			f.before = func(f *contentFixture, _ http.ResponseWriter, _ *http.Request, stage string) bool {
				if stage == "mime" {
					f.metadata = strings.Replace(f.metadata, tc.from, tc.to, 1)
				}
				return false
			}
			box := f.box(t)
			got, err := box.ReadMailMessageMIME(t.Context(), "f:2", "m:91", false)
			if !errors.Is(err, tc.want) {
				t.Fatal("unexpected scoped read result", err)
			}
			if tc.want == nil && (got.Parent != "f:2" || string(got.MIME) != f.mime) ||
				tc.want != nil && (got.ID != "" || len(got.MIME) != 0) {
				t.Fatal("inconsistent scoped MIME result")
			}
			if f.gets != 2 || f.bodies != 1 || f.modified != 0 || f.search != 0 {
				t.Fatal("single-message MIME lookup did not bracket its one content read")
			}
		})
	}
	t.Run("oversized message before download", func(t *testing.T) {
		f := newContentFixture()
		f.metadata = strings.Replace(f.metadata, `s="120"`, `s="1048577"`, 1)
		box := f.box(t)
		if _, err := box.ReadMailMessageMIME(t.Context(), "f:2", "m:91", false); !errors.Is(err, core.ErrUnsupported) || f.bodies != 0 {
			t.Fatal("oversized envelope was not rejected before MIME access", err)
		}
	})
}

func TestMailContentHEADReusesTLSConnection(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Zimbra-Modified", "1794736800123")
				w.Header().Set("X-Zimbra-Flags", "u")
				fmt.Fprint(w, strings.Repeat("message contents", 32768))
			}, Config{})
			s := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)}
			connections, requests := 0, 0
			ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				requests++
				if !info.Reused {
					connections++
				}
			}})
			for range 4 {
				stamp, flags, err := s.mailContentInfo(ctx, method, url.Values{"id": {"42"}, "sync": {"1"}})
				if err != nil || stamp.UnixMilli() != 1794736800123 || flags != "u" {
					t.Fatalf("metadata read = %v, %q, %v", stamp, flags, err)
				}
			}
			want := 1
			if method == http.MethodGet {
				want = 4
			}
			if connections != want || requests != 4 {
				t.Fatalf("%d HTTP requests and %d new TLS connections, want 4 and %d", requests, connections, want)
			}
		})
	}
}
