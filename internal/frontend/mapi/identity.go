// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"

	"ewsgo/internal/core"
)

// Namespace identifies a gateway installation, independently of any backend.
// Its configured UUID must survive restarts. This provisional mapping is not
// an Exchange directory, a database schema, or proof of mailbox access.
type Namespace struct {
	uuid [16]byte
	id   string
}

type MailboxIdentity struct {
	GUID, LegacyDN, RoutingID string
}

func NewNamespace(id string) (Namespace, error) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return Namespace{}, errors.New("deployment ID must be a nonzero UUID")
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 {
		return Namespace{}, errors.New("deployment ID must be a nonzero UUID")
	}
	var uuid [16]byte
	copy(uuid[:], raw)
	if uuid == [16]byte{} {
		return Namespace{}, errors.New("deployment ID must be a nonzero UUID")
	}
	return Namespace{uuid: uuid, id: formatUUID(uuid[:])}, nil
}

func (n Namespace) DeploymentID() string { return n.id }

// AddressBookGUID identifies a gateway directory object, not a native LDAP
// objectGUID, mailbox/store GUID or temporary NSPI ServerGuid. Its independent
// UUIDv5 domain was approved for backend-independent, restart-stable identity.
// Return GUID wire order: the first three integer fields are little-endian.
func (n Namespace) AddressBookGUID(scope core.Scope) ([16]byte, error) {
	if n.id == "" || scope.Backend == "" || scope.Account == "" || len(scope.Backend) > 1024 || len(scope.Account) > 4096 {
		return [16]byte{}, core.ErrInvalid
	}
	h := sha1.New()
	_, _ = h.Write(n.uuid[:])
	for _, part := range []string{"ewsgo/addressbook/v1", scope.Backend, scope.Account} {
		_, _ = h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(part))))
		_, _ = h.Write([]byte(part))
	}
	raw := h.Sum(nil)[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	return [16]byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6], raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]}, nil
}

func (n Namespace) Mailbox(identity core.Identity) (MailboxIdentity, error) {
	if n.id == "" || identity.Scope.Backend == "" || identity.Scope.Account == "" || len(identity.Scope.Backend) > 1024 || len(identity.Scope.Account) > 4096 {
		return MailboxIdentity{}, core.ErrInvalid
	}
	address, err := mail.ParseAddress(identity.Name)
	if err != nil || address.Address != identity.Name || len(identity.Name) > 320 {
		return MailboxIdentity{}, core.ErrInvalid
	}
	at := strings.LastIndexByte(identity.Name, '@')
	if at < 1 || at == len(identity.Name)-1 {
		return MailboxIdentity{}, core.ErrInvalid
	}
	// UUIDv5 uses SHA-1 for namespacing, never authentication. Length prefixes
	// keep backend/account boundaries unambiguous. SMTP renames keep GUID/DN.
	h := sha1.New()
	_, _ = h.Write(n.uuid[:])
	for _, part := range []string{"ewsgo/mailbox/v1", identity.Scope.Backend, identity.Scope.Account} {
		_, _ = h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(part))))
		_, _ = h.Write([]byte(part))
	}
	raw := h.Sum(nil)[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	id := formatUUID(raw)
	return MailboxIdentity{
		GUID:      id,
		LegacyDN:  "/o=EWSGo/ou=" + n.id + "/cn=Recipients/cn=" + id,
		RoutingID: id + "@" + strings.ToLower(identity.Name[at+1:]),
	}, nil
}

func mailboxGUIDWire(id string) ([16]byte, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 {
		return [16]byte{}, core.ErrInvalid
	}
	return [16]byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6], raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]}, nil
}

func formatUUID(raw []byte) string {
	s := hex.EncodeToString(raw)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
