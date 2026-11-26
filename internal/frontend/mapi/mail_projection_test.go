// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func projectSyntheticMail(t *testing.T, raw string) projectedMessage {
	t.Helper()
	o := messageTestObject("synthetic", "synthetic-folder")
	o.MIME = []byte(raw)
	p, err := projectMailContents(o)
	if err != nil {
		t.Fatal("synthetic projection failed", err)
	}
	return p
}
func projectionValue(values contentsValues, tag uint32) core.Value {
	return values[fixedContentsProperty(tag)]
}

func TestMailProjectionFollowUpProperties(t *testing.T) {
	completed := time.Date(2026, 11, 25, 11, 23, 58, 0, time.UTC)
	for _, test := range []struct {
		name  string
		props core.Properties
		want  map[uint32]core.Value
	}{
		{"unflagged", core.Properties{core.IsFlagged: core.Bool(false)}, nil},
		{"flagged", core.Properties{core.IsFlagged: core.Bool(true)}, map[uint32]core.Value{
			flagStatusTag: core.Integer(2), followupIconTag: core.Integer(6),
		}},
		{"completed", core.Properties{core.FollowUpCompleted: core.Bool(true), core.FollowUpCompleteAt: core.Timestamp(completed)}, map[uint32]core.Value{
			flagStatusTag: core.Integer(1), flagCompleteTimeTag: core.Timestamp(completed.Truncate(time.Minute)),
		}},
		{"undated completion", core.Properties{core.FollowUpCompleted: core.Bool(true)}, map[uint32]core.Value{
			flagStatusTag: core.Integer(1),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := core.Object{Kind: core.Message, Properties: test.props, MIME: []byte("Subject: fixture\r\n\r\nMessage\r\n")}
			projection, err := projectMailContents(object)
			if err != nil {
				t.Fatal(err)
			}
			for _, tag := range []uint32{flagStatusTag, flagCompleteTimeTag, followupIconTag} {
				if got := projectionValue(projection.values, tag); got != test.want[tag] {
					t.Fatalf("tag %08x: got %v, want %v", tag, got, test.want[tag])
				}
				fromTable := tableCell(projection.values, tag, nil, 1252)
				fromMessage := openedMessageProperty(messageRow{object: object}, storeBinding{}, tag&0xffff0000, true, 1252, false, 0)
				if fromMessage.kind != fromTable.kind || fromMessage.code != fromTable.code || !bytes.Equal(fromMessage.data, fromTable.data) {
					t.Fatalf("tag %08x: table and opened-message values disagree", tag)
				}
			}
		})
	}
}

func TestMailProjectionPlainUTF8HeadersAndNativeRecipients(t *testing.T) {
	o := messageTestObject("synthetic", "synthetic-folder")
	o.Recipients = []core.Recipient{{Kind: core.RecipientTo, Address: "to@example.invalid", DisplayName: "A To"}, {Kind: core.RecipientCc, Address: "cc@example.invalid", DisplayName: "A Cc"}, {Kind: core.RecipientBcc, Address: "bcc@example.invalid"}}
	o.MIME = []byte("From: =?UTF-8?B?w4nDqQ==?= <sender@example.invalid>\r\nTo: to@example.invalid\r\nSubject: =?windows-1252?Q?Prix_=80?=\r\nMessage-ID: <synthetic@example.invalid>\r\nDate: Sat, 14 Nov 2026 12:00:00 +0100\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nV=C3=A9rification =F0=9F=98=80\r\n")
	p, err := projectMailContents(o)
	if err != nil {
		t.Fatal("projection unavailable", err)
	}
	for tag, want := range map[uint32]core.Value{0x0037001f: core.Text("Prix €"), 0x1000001f: core.Text("Vérification 😀\r\n"), 0x10160003: core.Integer(1), 0x0042001f: core.Text("Éé"), 0x0c1a001f: core.Text("Éé"), 0x5d01001f: core.Text("sender@example.invalid"), 0x0e1b000b: core.Bool(false)} {
		if projectionValue(p.values, tag) != want {
			t.Fatal("property mapping mismatch", fmt.Sprintf("%08x", tag))
		}
	}
	if len(p.recipients) != 3 || len(p.attachments) != 0 || p.size != len(o.MIME) {
		t.Fatal("incomplete message structure")
	}
	for i, row := range p.recipients {
		if projectionValue(row, 0x0c150003) != core.Integer(i+1) || projectionValue(row, 0x3003001f) != core.Text(o.Recipients[i].Address) {
			t.Fatal("recipient role/address lost")
		}
	}
	if strings.Contains(fmt.Sprintf("%v %#v", p, p), "sender@example.invalid") {
		t.Fatal("projection formatting exposed content")
	}
}

func TestMailProjectionMultipartAlternativeMixedRelatedAndAttachments(t *testing.T) {
	raw := "From: sender@example.invalid\r\nContent-Type: multipart/mixed; boundary=mixed\r\n\r\n" +
		"--mixed\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCaf=E9\r\n" +
		"--alt\r\nContent-Type: multipart/related; boundary=rel; type=\"text/html\"; start=\"<root>\"\r\n\r\n" +
		"--rel\r\nContent-Type: image/png\r\nContent-ID: <image>\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC/w==\r\n" +
		"--rel\r\nContent-Type: text/html; charset=windows-1252\r\nContent-ID: <root>\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<meta charset=3Dwindows-1252><p>Prix =80<img src=3D\"cid:image\"></p>\r\n" +
		"--rel--\r\n--alt--\r\n" +
		"--mixed\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=utf-8''caf%C3%A9.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nECAwQA==\r\n--mixed--\r\n"
	// Related resources are an integral part of an alternative body, so they
	// must be preserved when that body is selected, rather than rejected/lost.
	p := projectSyntheticMail(t, raw)
	if projectionValue(p.values, 0x1000001f) != core.Text("Café") || projectionValue(p.values, 0x3fde0003) != core.Integer(1252) || projectionValue(p.values, 0x10160003) != core.Integer(3) || len(p.attachments) != 2 {
		t.Fatal("multipart bodies/resources not preserved")
	}
	html, ok := projectionValue(p.values, 0x10130102).(core.Binary)
	if !ok || !bytes.Contains(html, []byte{0x80}) {
		t.Fatal("HTML bytes disagree with original charset")
	}
	first, second := p.attachments[0], p.attachments[1]
	if !bytes.Equal(projectionValue(first, 0x37010102).(core.Binary), []byte{0, 1, 2, 255}) || projectionValue(first, 0x3712001f) != core.Text("image") || projectionValue(first, 0x37140003) != core.Integer(4) || projectionValue(second, 0x3707001f) != core.Text("café.bin") || !bytes.Equal(projectionValue(second, 0x37010102).(core.Binary), []byte{16, 32, 48, 64}) {
		t.Fatal("attachment data/name/CID lost")
	}
}

func TestMailProjectionRelatedMissingTypeUsesExplicitTextRoot(t *testing.T) {
	for _, start := range []bool{false, true} {
		parameter, parts := "", ""
		resource := "--b\r\nContent-Type: image/png\r\nContent-ID: <resource>\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC/w==\r\n"
		root := "--b\r\nContent-Type: text/html; charset=utf-8\r\nContent-ID: <root>\r\n\r\n<p>Body<img src=\"cid:resource\"></p>\r\n"
		if start {
			parameter, parts = "; start=\"<root>\"", resource+root
		} else {
			parts = root + resource
		}
		p := projectSyntheticMail(t, "Content-Type: multipart/related; boundary=b"+parameter+"\r\n\r\n"+parts+"--b--\r\n")
		if !bytes.Equal(projectionValue(p.values, 0x10130102).(core.Binary), []byte("<p>Body<img src=\"cid:resource\"></p>")) || len(p.attachments) != 1 || !bytes.Equal(projectionValue(p.attachments[0], 0x37010102).(core.Binary), []byte{0, 1, 2, 255}) || projectionValue(p.attachments[0], 0x3712001f) != core.Text("resource") {
			t.Fatal("missing-type fallback lost the explicit HTML root or inline bytes")
		}
	}
	for _, tc := range []struct{ parameter, root string }{
		{"; type=\"text/plain\"", "Content-Type: text/html\r\n\r\nhtml"},
		{"; type=\"\"", "Content-Type: text/html\r\n\r\nhtml"},
		{"", "Content-Type: application/octet-stream\r\n\r\nopaque"},
		{"", "\r\nmissing explicit root type"},
	} {
		o := messageTestObject("synthetic", "synthetic-folder")
		o.MIME = []byte("Content-Type: multipart/related; boundary=b" + tc.parameter + "\r\n\r\n--b\r\n" + tc.root + "\r\n--b--\r\n")
		if _, err := projectMailContents(o); err == nil {
			t.Fatal("missing-type fallback accepted a contradictory or unsupported root")
		}
	}
}

func TestMailProjectionRelatedMissingTypeWithAlternativeRoot(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n" +
		"--a\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlain body\r\n" +
		"--a\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>HTML body<img src=\"cid:image\"></p>\r\n--a--\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <image>\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC/w==\r\n--b--\r\n"
	p := projectSyntheticMail(t, raw)
	if projectionValue(p.values, 0x1000001f) != core.Text("Plain body") || !bytes.Equal(projectionValue(p.values, 0x10130102).(core.Binary), []byte("<p>HTML body<img src=\"cid:image\"></p>")) || len(p.attachments) != 1 || !bytes.Equal(projectionValue(p.attachments[0], 0x37010102).(core.Binary), []byte{0, 1, 2, 255}) || projectionValue(p.attachments[0], 0x37140003) != core.Integer(4) {
		t.Fatal("missing-type alternative root lost body variants or related resource")
	}
}

func TestMailProjectionAlternativeLastRepresentationAndMixedIndependentPart(t *testing.T) {
	p := projectSyntheticMail(t, "Content-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--b\r\nContent-Type: text/plain\r\n\r\nlast\r\n--b--\r\n")
	if projectionValue(p.values, 0x1000001f) != core.Text("last") {
		t.Fatal("alternative order ignored")
	}
	p = projectSyntheticMail(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: application/octet-stream\r\n\r\nfirst file\r\n--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n--b\r\nContent-Type: text/plain\r\n\r\nsecond file\r\n--b--\r\n")
	if projectionValue(p.values, 0x1000001f) != core.Text("body") || len(p.attachments) != 2 || string(projectionValue(p.attachments[1], 0x37010102).(core.Binary)) != "second file" {
		t.Fatal("independent mixed part overwritten/dropped")
	}
}

func TestMailProjectionCalendarAlternativePreservesBodyAndICS(t *testing.T) {
	const ics = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nEND:VCALENDAR"
	p := projectSyntheticMail(t, "Content-Type: multipart/alternative; boundary=b\r\n\r\n"+
		"--b\r\nContent-Type: text/plain\r\n\r\nInvitation body\r\n"+
		"--b\r\nContent-Type: text/calendar; method=REQUEST; charset=UTF-8\r\n\r\n"+ics+"\r\n--b--\r\n")
	if projectionValue(p.values, 0x1000001f) != core.Text("Invitation body") ||
		projectionValue(p.values, 0x001a001f) != core.Text("IPM.Note") || len(p.attachments) != 1 {
		t.Fatal("calendar alternative changed the normal mail view")
	}
	attachment := p.attachments[0]
	if projectionValue(attachment, 0x3707001f) != core.Text("calendar.ics") ||
		projectionValue(attachment, 0x370e001f) != core.Text("text/calendar") ||
		!bytes.Equal(projectionValue(attachment, 0x37010102).(core.Binary), []byte(ics)) {
		t.Fatal("calendar alternative lost its original decoded content")
	}
}

const embeddedMailFixture = "Subject: Outer subject\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
	"--outer\r\nContent-Type: text/plain\r\n\r\nOuter body\r\n" +
	"--outer\r\nContent-Type: message/rfc822\r\n\r\n" +
	"From: sender@example.invalid\r\nTo: Example <inner@example.invalid>\r\nSubject: Attached subject\r\n" +
	"Content-Type: multipart/mixed; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nAttached body\r\n" +
	"--inner\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAQID\r\n--inner--\r\n--outer--\r\n"

func TestMailProjectionEmbeddedMail(t *testing.T) {
	p := projectSyntheticMail(t, embeddedMailFixture)
	if len(p.attachments) != 1 || len(p.embedded) != 1 || projectionValue(p.values, 0x1000001f) != core.Text("Outer body") {
		t.Fatal("outer message lost its body or embedded attachment")
	}
	at := p.attachments[0]
	if projectionValue(at, 0x37050003) != core.Integer(5) || projectionValue(at, 0x37010102) != nil ||
		projectionValue(at, 0x3001001f) != core.Text("Attached subject") {
		t.Fatal("embedded message exposed as a binary attachment")
	}
	inside := p.embedded[0]
	if len(inside.recipients) != 1 || len(inside.attachments) != 1 || projectionValue(inside.values, 0x1000001f) != core.Text("Attached body") ||
		projectionValue(inside.recipients[0], 0x3003001f) != core.Text("inner@example.invalid") {
		t.Fatal("embedded message lost its body, recipient, or attachment")
	}
}

func TestMailProjectionEmbeddedDepthLimit(t *testing.T) {
	raw := strings.Repeat("Content-Type: message/rfc822\r\n\r\n", maxMailPartDepth+1) + "Subject: Last\r\n\r\nbody"
	o := messageTestObject("synthetic", "folder")
	o.MIME = []byte(raw)
	if _, err := projectMailContents(o); err == nil {
		t.Fatal("embedded messages bypassed the MIME nesting limit")
	}
}

func TestMailProjectionRejectsUnsupportedMalformedAndOverBudget(t *testing.T) {
	for _, raw := range []string{
		"Subject: duplicate\r\nSubject: duplicate\r\n\r\ntext",
		"From: one@example.invalid, two@example.invalid\r\n\r\ntext",
		"Date: invalid\r\n\r\ntext",
		"Content-Type: text/plain; charset=unknown\r\n\r\ntext",
		"Content-Type: text/plain; charset=utf-8\r\n\r\n\xff",
		"Content-Type: text/plain\r\n\r\nnonascii\xff",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n%%%",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nbad=G0",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: 7bit\r\n\r\n\xff",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\ntruncated",
		"Content-Type: multipart/signed; boundary=b\r\n\r\n--b--\r\n",
		"Content-Type: message/rfc822\r\n\r\ninvalid embedded headers",
		"Content-Type: message/partial\r\n\r\nFrom: embedded@example.invalid\r\n\r\nbody",
		"Content-Type: application/ms-tnef\r\n\r\nopaque",
		"Content-Type: application/pkcs7-mime\r\n\r\nopaque",
		"Content-Type: multipart/related; boundary=b; type=\"text/html\"; start=\"<missing>\"\r\n\r\n--b\r\nContent-Type: text/html\r\nContent-ID: <other>\r\n\r\ntext\r\n--b--\r\n",
		"Subject: " + strings.Repeat("x", maxMailHeaderBytes) + "\r\n\r\nbody",
		"Subject: synthetic\r\n\r\n" + strings.Repeat("x", core.MaxMailContentMIMEBytes),
		"Subject: synthetic\r\n\r\nzero\x00",
	} {
		o := messageTestObject("synthetic", "folder")
		o.MIME = []byte(raw)
		got, err := projectMailContents(o)
		if err == nil || got.values != nil || got.attachments != nil {
			t.Fatal("invalid MIME yielded usable projection")
		}
	}
	for _, mode := range []string{"parts", "attachments", "depth"} {
		var raw string
		if mode == "depth" {
			for i := 0; i < maxMailPartDepth+2; i++ {
				raw += fmt.Sprintf("Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
			}
			raw += "Content-Type: text/plain\r\n\r\nbody\r\n"
			for i := maxMailPartDepth + 1; i >= 0; i-- {
				raw += fmt.Sprintf("--b%d--\r\n", i)
			}
		} else {
			raw = "Content-Type: multipart/mixed; boundary=b\r\n\r\n"
			kind, count := "text/plain", maxMailParts+1
			if mode == "attachments" {
				kind, count = "application/octet-stream", maxMailAttachments+1
			}
			for i := 0; i < count; i++ {
				raw += "--b\r\nContent-Type: " + kind + "\r\n\r\npart\r\n"
			}
			raw += "--b--\r\n"
		}
		o := messageTestObject("synthetic", "folder")
		o.MIME = []byte(raw)
		if _, err := projectMailContents(o); err == nil {
			t.Fatal("MIME quota ignored", mode)
		}
	}
}

func FuzzMailProjection(f *testing.F) {
	for _, raw := range []string{"Subject: seed\r\n\r\nbody", "Content-Type: text/html; charset=utf-8\r\n\r\n<p>seed</p>", "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nbody\r\n--b--\r\n"} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		o := messageTestObject("synthetic", "folder")
		o.MIME = raw
		p, err := projectMailContents(o)
		if err == nil && (p.bytes > maxMailProjectionBytes || len(p.attachments) > maxMailAttachments || len(p.recipients) > 256) {
			t.Fatal("successful projection exceeded limits")
		}
	})
}
