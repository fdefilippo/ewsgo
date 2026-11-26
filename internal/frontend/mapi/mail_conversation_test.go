// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestMailConversationSubjectNormalizationAndGrouping(t *testing.T) {
	for _, tc := range []struct{ subject, prefix, normalized string }{
		{"Topic", "", "Topic"}, {"Re:   Topic", "Re: ", "Topic"},
		{"AW:Topic", "AW: ", "Topic"}, {"Fwd: Topic", "Fwd: ", "Topic"},
		{"R2: Topic", "", "R2: Topic"}, {"Long: Topic", "", "Long: Topic"},
		{"Re : Topic", "", "Re : Topic"}, {"Ré: Topic", "Ré: ", "Topic"},
		{"Re: Re: Topic", "Re: ", "Re: Topic"}, {"", "", ""},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			p := projectSyntheticMail(t, "Subject: "+tc.subject+"\r\n\r\nbody")
			for tag, want := range map[uint32]core.Value{
				0x003d001f: core.Text(tc.prefix), 0x0e1d001f: core.Text(tc.normalized),
				0x0070001f: core.Text(tc.normalized), 0x3016000b: core.Bool(false),
			} {
				if projectionValue(p.values, tag) != want {
					t.Fatalf("missing or incorrect synthetic conversation property %08x", tag)
				}
			}
		})
	}
	base := projectSyntheticMail(t, "Subject: Topic\r\n\r\nbody")
	reply := projectSyntheticMail(t, "Subject: RE: topic\r\n\r\nreply")
	other := projectSyntheticMail(t, "Subject: Different\r\n\r\nbody")
	// MD5 of uppercase TOPIC, UTF-16LE, no terminating NUL. Fixed vector,
	// independent of the implementation's normalization/hash helpers.
	want, _ := hex.DecodeString("5f49134c1c67953beedbe8c4c4f5bb42")
	id, ok := projectionValue(base.values, 0x30130102).(core.Binary)
	if !ok || !bytes.Equal(id, want) || !bytes.Equal(id, projectionValue(reply.values, 0x30130102).(core.Binary)) || bytes.Equal(id, projectionValue(other.values, 0x30130102).(core.Binary)) {
		t.Fatal("conversation identity collapsed different subjects or split a reply")
	}
}

func TestMailConversationThreadHeadersAndInvalidIndex(t *testing.T) {
	index := append([]byte{1, 0, 0, 0, 0, 0}, []byte("0123456789abcdef")...)
	p := projectSyntheticMail(t, "Subject: Different display subject\r\nThread-Topic: =?UTF-8?Q?Original_topic?=\r\nThread-Index: "+base64.StdEncoding.EncodeToString(index)+"\r\n\r\nbody")
	if projectionValue(p.values, 0x0070001f) != core.Text("Original topic") || projectionValue(p.values, 0x3016000b) != core.Bool(true) || !bytes.Equal(projectionValue(p.values, 0x30130102).(core.Binary), index[6:]) || !bytes.Equal(projectionValue(p.values, 0x00710102).(core.Binary), index) {
		t.Fatal("MIME conversation headers not preserved")
	}
	for _, encoded := range []string{"not valid base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		p := projectSyntheticMail(t, "Subject: Topic\r\nThread-Index: "+encoded+"\r\n\r\nbody")
		if projectionValue(p.values, 0x3016000b) != core.Bool(false) || len(projectionValue(p.values, 0x30130102).(core.Binary)) != 16 {
			t.Fatal("invalid index prevented topic fallback")
		}
	}
	p = projectSyntheticMail(t, "Thread-Topic: Topic only\r\n\r\nbody")
	if projectionValue(p.values, 0x0070001f) != core.Text("Topic only") || projectionValue(p.values, 0x0e1d001f) != nil {
		t.Fatal("topic-only message invented a subject")
	}
	p = projectSyntheticMail(t, "Content-Type: text/plain\r\n\r\nbody")
	if projectionValue(p.values, 0x0070001f) != nil || projectionValue(p.values, 0x30130102) != nil {
		t.Fatal("subjectless message assigned a shared conversation")
	}
	p = projectSyntheticMail(t, "Subject: Localized reply: Original topic\r\nThread-Topic: Original topic\r\n\r\nbody")
	if projectionValue(p.values, 0x0e1d001f) != core.Text("Original topic") || projectionValue(p.values, 0x003d001f) != core.Text("Localized reply: ") {
		t.Fatal("explicit topic suffix not used to normalize a localized reply")
	}
	for _, header := range []string{"Thread-Topic: a\r\nThread-Topic: b", "Thread-Index: YQ==\r\nThread-Index: Yg=="} {
		o := messageTestObject("synthetic", "synthetic-folder")
		o.MIME = []byte(header + "\r\n\r\nbody")
		if _, err := projectMailContents(o); err == nil {
			t.Fatal("ambiguous repeated conversation header accepted")
		}
	}
}

func TestMailConversationLongSubjectsRemainReadableAndHashIsBounded(t *testing.T) {
	subject := strings.Repeat("a", 300)
	p := projectSyntheticMail(t, "Subject: "+subject+"\r\n\r\nbody")
	if projectionValue(p.values, 0x0037001f) != core.Text(subject) || projectionValue(p.values, 0x0e1d001f) != nil || projectionValue(p.values, 0x0070001f) != core.Text(subject) {
		t.Fatal("long subject rejected, truncated or projected outside normalized limit")
	}
	q := projectSyntheticMail(t, "Subject: "+subject[:255]+"different tail\r\n\r\nbody")
	if !bytes.Equal(projectionValue(p.values, 0x30130102).(core.Binary), projectionValue(q.values, 0x30130102).(core.Binary)) {
		t.Fatal("conversation hash did not use the first 255 UTF-16 units")
	}
	for _, tag := range []uint32{0x003d001f, 0x0e1d001f, 0x0070001f, 0x00710102, 0x30130102, 0x3016000b} {
		if !contentsProjectionSchema(true, false)(fixedContentsProperty(tag)) || !contentsProjectionSchema(true, true)(fixedContentsProperty(tag)) {
			t.Fatal("native conversation property absent from the selection schema")
		}
	}
}

func TestMailConversationUnicodeHashVectors(t *testing.T) {
	// Vectors calculated independently with Python hashlib over uppercase
	// UTF-16LE bytes, including the 255-unit boundary within a surrogate pair.
	for _, tc := range []struct{ subject, digest string }{
		{"ié😀", "fcdde0152bff2fa7dcd65ff171b0a971"},
		{strings.Repeat("😀", 128), "0d478241f644cb58e54e4709cfacd788"},
	} {
		p := projectSyntheticMail(t, "Subject: "+tc.subject+"\r\n\r\nbody")
		want, _ := hex.DecodeString(tc.digest)
		if !bytes.Equal(projectionValue(p.values, 0x30130102).(core.Binary), want) {
			t.Fatal("Unicode conversation digest differs from the independent vector")
		}
	}
}
