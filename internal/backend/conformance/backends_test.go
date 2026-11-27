// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"os"
	"testing"

	"ewsgo/internal/backend/conformance"
	"ewsgo/internal/backend/jmap"
	"ewsgo/internal/backend/memory"
	"ewsgo/internal/backend/zimbra"
	"ewsgo/internal/core"
	"ewsgo/internal/labconfig"
)

func TestBackendMailContract(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	t.Run("memory", func(t *testing.T) {
		const messageID = "<backend-contract-memory@example.invalid>"
		const name = "contract.bin"
		raw := []byte("Message-ID: " + messageID + "\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=contract\r\n\r\n" +
			"--contract\r\nContent-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment; filename=" + name + "\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString(payload) + "\r\n--contract--\r\n")
		provider := memory.New()
		identity := core.Identity{Scope: core.Scope{Backend: "memory", Account: "contract"}, Name: "contract"}
		open := func(ctx context.Context) (core.Session, error) {
			box, err := provider.Open(ctx, identity.Scope)
			return memorySession{identity, box}, err
		}
		session, err := open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		box := session.Mailbox()
		inbox := put(t, box, core.Target{Kind: core.Folder, Parent: box.Root()},
			core.Patch{Set: core.Properties{core.WellKnown: core.Text("inbox")}})
		attachments := []core.Attachment{{ID: "contract", MediaType: "application/octet-stream", Data: payload}}
		put(t, box, core.Target{Kind: core.Message, Parent: inbox.ID}, core.Patch{
			Set:  core.Properties{core.MessageID: core.Text(messageID)},
			MIME: &raw, Attachments: &attachments,
		})
		conformance.Mail(t, conformance.Fixture{Open: open, MessageID: messageID,
			AttachmentName: name, AttachmentContent: payload, MIME: raw})
	})
	t.Run("zimbra", func(t *testing.T) {
		credentials, ca := liveConfig(t, "EWSGO_CONFORMANCE_ZIMBRA")
		client, err := zimbra.New(zimbra.Config{Origin: credentials.URL, RootCAs: ca})
		if err != nil {
			t.Fatal(err)
		}
		messageID := os.Getenv("EWSGO_CONFORMANCE_ZIMBRA_MESSAGE_ID")
		if messageID == "" {
			messageID = "<backend-contract-zimbra@example.invalid>"
		}
		conformance.Mail(t, conformance.Fixture{Open: login(client, credentials),
			MessageID: messageID, AttachmentName: "fixture.bin", AttachmentContent: payload})
	})
	t.Run("jmap", func(t *testing.T) {
		credentials, ca := liveConfig(t, "EWSGO_CONFORMANCE_JMAP")
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12}
		t.Cleanup(transport.CloseIdleConnections)
		client, err := jmap.New(credentials.URL, transport)
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if path := os.Getenv("EWSGO_CONFORMANCE_JMAP_MIME"); path != "" {
			raw, err = os.ReadFile(path)
			if err != nil || len(bytes.TrimSpace(raw)) == 0 {
				t.Fatal("could not read MIME fixture")
			}
		}
		conformance.Mail(t, conformance.Fixture{Open: login(client, credentials),
			MessageID: "<backend-contract-jmap@example.invalid>", AttachmentName: "contract.bin",
			AttachmentContent: payload, MIME: raw})
	})
}

type memorySession struct {
	identity core.Identity
	box      core.Mailbox
}

func (s memorySession) Identity() core.Identity   { return s.identity }
func (s memorySession) Mailbox() core.Mailbox     { return s.box }
func (memorySession) Close(context.Context) error { return nil }

func put(t *testing.T, box core.Mailbox, target core.Target, patch core.Patch) core.Object {
	t.Helper()
	edit, err := box.Begin(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer edit.Discard()
	if err := edit.Apply(patch); err != nil {
		t.Fatal(err)
	}
	obj, err := edit.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func liveConfig(t *testing.T, key string) (labconfig.Credentials, *x509.CertPool) {
	t.Helper()
	credentialsPath := os.Getenv(key + "_CREDENTIALS")
	if credentialsPath == "" {
		t.Skip(key + "_CREDENTIALS requires an isolated synthetic mailbox")
	}
	credentials, err := labconfig.Read(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	caPath := os.Getenv(key + "_CA")
	if caPath == "" {
		return credentials, nil
	}
	pem, err := os.ReadFile(caPath)
	ca := x509.NewCertPool()
	if err != nil || !ca.AppendCertsFromPEM(pem) {
		t.Fatal("invalid backend CA")
	}
	return credentials, ca
}

func login(client core.Authenticator, credentials labconfig.Credentials) func(context.Context) (core.Session, error) {
	return func(ctx context.Context) (core.Session, error) {
		return client.Authenticate(ctx, credentials.User, credentials.Password)
	}
}
