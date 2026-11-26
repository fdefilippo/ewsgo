// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/mail"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestUncompressImportRTFReferenceVector(t *testing.T) {
	// MS-OXRTFCP §3.1.1 and §3.3.1.
	encoded, err := hex.DecodeString("2d0000002b0000004c5a4675f1c5c7a703000a007263706731323542320af32068656c090020627705b06c647d0a800fa0")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := uncompressImportRTF(encoded)
	if err != nil || string(raw) != "{\\rtf1\\ansi\\ansicpg1252\\pard hello world}\r\n" {
		t.Fatalf("reference vector decoded to %q: %v", raw, err)
	}
	badChecksum := bytes.Clone(encoded)
	badChecksum[12] ^= 1
	for _, invalid := range [][]byte{badChecksum, encoded[:len(encoded)-1]} {
		if _, err := uncompressImportRTF(invalid); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("bad RTF envelope passed validation", err)
		}
	}
	tooLarge := bytes.Clone(encoded)
	binary.LittleEndian.PutUint32(tooLarge[4:], maxMessageImportBytes+1)
	if _, err := uncompressImportRTF(tooLarge); !errors.Is(err, core.ErrLimit) {
		t.Fatal("oversized RTF envelope passed validation", err)
	}
}

func TestImportedMailMIMEPlainRTFWithoutTransportHeaders(t *testing.T) {
	body := "{\\rtf1\\ansi\\fromtext\\ansicpg1252\\deff0{\\fonttbl{\\f0\\fnil Arial;}}\\pard\\plain " +
		"Pi\\'f9 \\{chiaro\\}\\tab \\u-10179?\\u-8704?\\par\r\nseconda riga}"
	date := time.Date(2026, 11, 24, 6, 15, 1, 0, time.UTC)
	fixture := func(rtf, address string) []byte {
		stream := &fastTransfer{limit: maxMessageImportBytes}
		stream.scalar(0x001a001f, core.Text("IPM.Note"))
		stream.scalar(0x003d001f, core.Text("RE: "))
		stream.scalar(0x0e1d001f, core.Text("Oggetto è"))
		stream.scalar(0x5d02001f, core.Text(address))
		stream.scalar(0x0042001f, core.Text("Français"))
		stream.scalar(0x0c1e001f, core.Text("SMTP"))
		stream.scalar(0x0c1f001f, core.Text("sender@example.test"))
		stream.scalar(0x1035001f, core.Text("<rtf@example.test>"))
		stream.scalar(0x00390040, core.Timestamp(date))
		stream.scalar(0x0e070003, core.Integer(1))
		stream.scalar(messageNativeBody, core.Integer(2))
		stream.scalar(messageRTFTag, core.Binary(uncompressedRTFEnvelope([]byte(rtf))))
		return stream.data
	}
	raw, options, err := importedMailMIME(fixture(body, "author@example.test"), nil, nil)
	if err != nil || !options.Read {
		t.Fatal("RTF import failed", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	from, err := parsed.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Address != "author@example.test" || from[0].Name != "Français" {
		t.Fatal("represented author was lost", err)
	}
	sender, err := parsed.Header.AddressList("Sender")
	if err != nil || len(sender) != 1 || sender[0].Address != "sender@example.test" {
		t.Fatal("sender was lost", err)
	}
	gotDate, err := parsed.Header.Date()
	if err != nil || !gotDate.Equal(date) || parsed.Header.Get("Message-ID") != "<rtf@example.test>" {
		t.Fatal("native headers were lost", err)
	}
	projected, err := projectMailContents(core.Object{Kind: core.Message, MIME: raw})
	if err != nil || projectionValue(projected.values, 0x1000001f) != core.Text("Più {chiaro}\t😀\r\nseconda riga") ||
		projectionValue(projected.values, 0x0037001f) != core.Text("RE: Oggetto è") {
		t.Fatal("plain text or subject round trip failed", err)
	}
	for _, tc := range []struct {
		rtf, address string
		err          error
	}{
		{body[:len(body)-1], "author@example.test", core.ErrInvalid},
		{body, "author@example.test\r\nX-Added: value", core.ErrInvalid},
		{"{\\rtf1\\ansi\\fromhtml1 ignored}", "author@example.test", core.ErrUnsupported},
		{"{\\rtf1\\ansi\\fromtext{\\*\\htmltag1 ignored}}", "author@example.test", core.ErrUnsupported},
	} {
		if _, _, err := importedMailMIME(fixture(tc.rtf, tc.address), nil, nil); !errors.Is(err, tc.err) {
			t.Fatal("unsupported or malformed object passed validation", err)
		}
	}
}

func TestMailImportRoundTripsOriginalRichRTF(t *testing.T) {
	const rich = `{\rtf1\ansi\ansicpg1252\pard Test {\b Bold} and {\i Italic}\par End.}`
	oneOff := append([]byte("\x00\x00\x00\x00"+oneOffProvider), 0, 0, 0, 0x80)
	for _, field := range []string{"Sender", "SMTP", "sender@example.test"} {
		oneOff = append(oneOff, contentsUnicode(field)...)
	}
	stream := &fastTransfer{limit: maxMessageImportBytes}
	stream.scalar(0x001a001f, core.Text("IPM.Note"))
	stream.scalar(0x0e070003, core.Integer(0))
	stream.scalar(0x0c190102, core.Binary(oneOff))
	stream.scalar(messageRTFTag, core.Binary(uncompressedRTFEnvelope([]byte(rich))))
	raw, options, err := importedMailMIME(stream.data, nil, nil)
	if err != nil || options.Read {
		t.Fatal("rich RTF message failed to import", err)
	}
	projected, err := projectMailContents(core.Object{Kind: core.Message, MIME: raw})
	if err != nil || len(projected.attachments) != 0 || projectionValue(projected.values, messageNativeBody) != core.Integer(2) ||
		projectionValue(projected.values, 0x1000001f) != core.Text("Test Bold and Italic\r\nEnd.") {
		t.Fatal("the MIME alternatives did not project as a message body", err)
	}
	data, ok := messageBodyBytes(projected.values, messageRTFTag, 1252)
	got, err := uncompressImportRTF(data)
	if !ok || err != nil || string(got) != rich {
		t.Fatal("the original RTF bytes were lost", err)
	}
}

func TestMailImportResolvesAddressEntryIDs(t *testing.T) {
	const author = "author@example.test"
	const sender = "sender@example.test"
	const dn = "/o=EWSGo/ou=Laboratory/cn=Recipients/cn=sender"
	oneOff := append([]byte("\x00\x00\x00\x00"+oneOffProvider), 0, 0, 0, 0x80)
	for _, field := range []string{"Français", "SMTP", author} {
		oneOff = append(oneOff, contentsUnicode(field)...)
	}
	props := map[uint32]core.Value{
		0x00410102: core.Binary(oneOff),
		0x0c190102: core.Binary(mailUserEntryIDHeader + dn + "\x00"),
	}
	resolve := func(entry string) (string, error) {
		if entry != dn {
			return "", core.ErrNotFound
		}
		return sender, nil
	}
	doc, err := importMailHeaders("", props, resolve)
	if err != nil {
		t.Fatal(err)
	}
	from, err := mail.ParseAddress(doc.Header.Get("From"))
	if err != nil || from.Address != author || from.Name != "Français" {
		t.Fatalf("bad author: %v, %v", from, err)
	}
	actualSender, err := mail.ParseAddress(doc.Header.Get("Sender"))
	if err != nil || actualSender.Address != sender {
		t.Fatalf("bad sender: %v, %v", actualSender, err)
	}
	if _, err := importMailHeaders("", props, nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("an EX address without GAL resolution was accepted", err)
	}
	props[0x00410102] = core.Binary(oneOff[:len(oneOff)-1])
	if _, err := importMailHeaders("", props, resolve); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("an incomplete one-off address was accepted", err)
	}
}
