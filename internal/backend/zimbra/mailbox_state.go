// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"strings"

	"ewsgo/internal/core"
)

func (s *Session) outOfOffice(ctx context.Context) (bool, error) {
	const preference = "zimbraPrefOutOfOfficeReplyEnabled"
	request := element(accountNS, "GetPrefsRequest", "", attr(element(accountNS, "pref", ""), "name", preference))
	response, err := s.request(ctx, accountNS, "GetPrefs", "", request)
	if err != nil {
		return false, err
	}
	var found *node
	for i := range response.Children {
		pref := &response.Children[i]
		if pref.XMLName.Space != accountNS || pref.XMLName.Local != "pref" || pref.get("name") != preference || found != nil {
			return false, ErrProtocol
		}
		found = pref
	}
	if found == nil {
		// GetPrefs returns stored attributes only; this optional account-only
		// attribute is absent when unset. Zimbra 10 ZAttrAccount's getter
		// isPrefOutOfOfficeReplyEnabled explicitly defaults an unset value
		// to false. An explicit empty/malformed value still fails below.
		return false, nil
	}
	switch strings.ToLower(found.Text) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, ErrProtocol
	}
}

func (m *mailbox) MailboxOwner(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// The authenticated session constructs this mailbox from its own GetInfo
	// identity. Backend resource operations still authorize each native request.
	if _, err := m.native.credential(); err != nil {
		return false, semanticError(err)
	}
	return true, nil
}

func (m *mailbox) MailboxState(ctx context.Context) (core.MailboxState, error) {
	oof, err := m.native.outOfOffice(ctx)
	if err != nil {
		return core.MailboxState{}, semanticError(err)
	}
	// Sending through the provider uses the separate native draft operation.
	return core.MailboxState{Owner: true, CanSend: false, OutOfOffice: oof}, nil
}
