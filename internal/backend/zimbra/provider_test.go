// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"ewsgo/internal/core"
	"testing"
)

func TestAuthenticatedProviderUsesCanonicalIdentity(t *testing.T) {
	client := testClient(t, fakeZimbra(t), Config{})
	session, err := client.Authenticate(context.Background(), "alias@example.invalid", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	identity := session.Identity()
	if identity.Scope.Account != "synthetic-account" || identity.Name != "lab@example.invalid" {
		t.Fatal("request name was trusted instead of native identity")
	}
	page, err := session.Mailbox().Query(context.Background(), core.Query{Kind: core.Folder})
	if err != nil || len(page.Objects) != 2 {
		t.Fatal("native folders not mapped")
	}
	if page.Objects[1].Properties[core.WellKnown] != core.Text("inbox") {
		t.Fatal("distinguished semantics not mapped")
	}
	if session.Mailbox().Capabilities().Write {
		t.Fatal("unsupported write advertised")
	}
}
