// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package conformance provides the shared backend Mail contract checks.
package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

// Fixture identifies a synthetic message already in Inbox. MIME is optional
// because the transport can add or normalize headers when delivering a fixture.
type Fixture struct {
	Open              func(context.Context) (core.Session, error)
	MessageID         string
	AttachmentName    string
	AttachmentContent []byte
	MIME              []byte
}

// Mail checks the common Mail profile and the advertised attachment capability.
// Set up backend-specific fixtures before calling it.
func Mail(t *testing.T, fixture Fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	open := func() core.Session {
		t.Helper()
		session, err := fixture.Open(ctx)
		if err != nil {
			t.Fatalf("open mailbox: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := session.Close(ctx); err != nil {
				t.Errorf("close mailbox: %v", err)
			}
		})
		return session
	}
	session := open()
	identity := session.Identity()
	if identity.Scope.Account == "" || identity.Scope.Backend == "" || identity.Name == "" {
		t.Fatal("mailbox identity is incomplete")
	}
	box := session.Mailbox()
	root, err := box.Get(ctx, box.Root())
	if err != nil || root.Kind != core.Folder || root.ID != box.Root() || root.Parent != "" {
		t.Fatalf("root projection: %v", err)
	}
	inbox := find(t, ctx, box, core.Query{Parent: root.ID, Kind: core.Folder, Limit: 2}, core.WellKnown, "inbox")
	msg := find(t, ctx, box, core.Query{Parent: inbox.ID, Kind: core.Message, Limit: 3}, core.MessageID, fixture.MessageID)
	raw := readMIME(t, ctx, box, msg.ID)
	if len(fixture.MIME) != 0 && !bytes.Equal(raw, fixture.MIME) {
		t.Fatal("MIME differs from the imported fixture")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || parsed.Header.Get("Message-ID") != fixture.MessageID {
		t.Fatalf("MIME identity: %v", err)
	}
	checkMIMEAttachment(t, parsed, fixture)
	if box.Capabilities().Attachments {
		checkAttachment(t, ctx, box, msg, fixture)
	}
	if !box.Capabilities().Write {
		if _, err := box.Begin(ctx, core.Target{ID: msg.ID, Expected: msg.Revision}); !errors.Is(err, core.ErrUnsupported) {
			t.Fatalf("unadvertised generic edit: %v", err)
		}
	}
	if !box.Capabilities().Delete {
		if err := box.Delete(ctx, msg.ID, msg.Revision); !errors.Is(err, core.ErrUnsupported) {
			t.Fatalf("unadvertised generic delete: %v", err)
		}
	}
	second := open()
	if second.Identity() != identity || second.Mailbox().Root() != root.ID {
		t.Fatal("identity changed on a fresh login")
	}
	observed, err := second.Mailbox().Get(ctx, msg.ID)
	if err != nil || observed.ID != msg.ID || observed.Parent != inbox.ID || observed.Revision != msg.Revision {
		t.Fatalf("object identity or revision changed on a fresh login: %v", err)
	}
	if got := readMIME(t, ctx, second.Mailbox(), msg.ID); !bytes.Equal(got, raw) {
		t.Fatal("MIME changed on a fresh login")
	}
}

func find(t *testing.T, ctx context.Context, box core.Mailbox, q core.Query, key core.Key, value string) core.Object {
	t.Helper()
	seen := make(map[core.ID]bool)
	for range 1000 {
		page, err := box.Query(ctx, q)
		if err != nil {
			t.Fatalf("list mailbox objects: %v", err)
		}
		if len(page.Objects) > q.Limit || page.More && page.NextOffset <= q.Offset {
			t.Fatal("invalid or stuck paginated result")
		}
		for _, obj := range page.Objects {
			if obj.ID == "" || seen[obj.ID] || obj.Parent != q.Parent || obj.Kind != q.Kind {
				t.Fatal("invalid containment or duplicate object in paginated result")
			}
			seen[obj.ID] = true
			if obj.Properties[key] == core.Text(value) {
				return obj
			}
		}
		if !page.More {
			break
		}
		q.Offset = page.NextOffset
	}
	t.Fatal("required synthetic fixture is absent from the result")
	return core.Object{}
}

func readMIME(t *testing.T, ctx context.Context, box core.Mailbox, id core.ID) []byte {
	t.Helper()
	if reader, ok := box.(core.MIMEReader); ok {
		raw, err := reader.ReadMIME(ctx, id)
		if err != nil || len(raw) == 0 {
			t.Fatalf("read MIME: %v", err)
		}
		return raw
	}
	obj, err := box.Get(ctx, id)
	if err != nil || len(obj.MIME) == 0 {
		t.Fatalf("read embedded MIME: %v", err)
	}
	return obj.MIME
}

func checkMIMEAttachment(t *testing.T, parsed *mail.Message, fixture Fixture) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		t.Fatal("fixture is not multipart MIME")
	}
	parts := multipart.NewReader(parsed.Body, params["boundary"])
	for {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatalf("read MIME part: %v", err)
		}
		if part.FileName() != fixture.AttachmentName {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, 1<<20))
		_ = part.Close()
		if err != nil {
			t.Fatalf("read MIME attachment: %v", err)
		}
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "base64") {
			data, err = base64.StdEncoding.DecodeString(string(data))
		}
		if err != nil || !bytes.Equal(data, fixture.AttachmentContent) {
			t.Fatalf("MIME attachment bytes: %v", err)
		}
		return
	}
}

func checkAttachment(t *testing.T, ctx context.Context, box core.Mailbox, msg core.Object, fixture Fixture) {
	t.Helper()
	if reader, ok := box.(core.MailAttachmentReader); ok {
		parts, err := reader.ListMailAttachments(ctx, msg.ID)
		if err != nil {
			t.Fatalf("list attachments: %v", err)
		}
		for _, part := range parts {
			if part.Name != fixture.AttachmentName {
				continue
			}
			data, err := reader.ReadMailAttachment(ctx, msg.ID, part.ID)
			if err != nil || !bytes.Equal(data.Data, fixture.AttachmentContent) {
				t.Fatalf("attachment bytes: %v", err)
			}
			if _, err := reader.ReadMailAttachment(ctx, msg.ID, "unknown"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("unknown attachment: %v", err)
			}
			return
		}
		t.Fatal("attachment not listed")
	}
	obj, err := box.Get(ctx, msg.ID)
	if err != nil || len(obj.Attachments) != 1 || !bytes.Equal(obj.Attachments[0].Data, fixture.AttachmentContent) {
		t.Fatalf("embedded attachment: %v", err)
	}
}
