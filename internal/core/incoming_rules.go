// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxInboxRules = 64
const DefaultInboxRuleProvider = "RuleOrganizer"
const MaxInboxRuleProviderBytes = 64
const MaxInboxRuleProviderDataBytes = 255

// InboxRule is the subset of native incoming rules that the gateway can
// interpret as equivalent MAPI rules. The list carries evaluation order.
type InboxRule struct {
	Name            string
	Enabled         bool
	SubjectContains string
	Stop            bool
	Provider        string
	ProviderData    []byte
	// A rule without deferred actions always marks the message as read.
	// With deferred actions, this field adds the native action before them.
	MarkRead        bool
	DeferredActions []byte
	DeferredRuleID  uint64
}

func (r InboxRule) Valid() bool {
	return validRuleText(r.Name, false) && validRuleText(r.SubjectContains, true) &&
		ValidInboxRuleProvider(r.Provider) && len(r.ProviderData) <= MaxInboxRuleProviderDataBytes &&
		(len(r.DeferredActions) == 0 && r.DeferredRuleID == 0 || r.DeferredRuleID != 0 && ValidDeferredActions(r.DeferredActions))
}

func ValidInboxRuleProvider(s string) bool {
	return s == "" || len(s) <= MaxInboxRuleProviderBytes && strings.HasPrefix(s, DefaultInboxRuleProvider) && validRuleText(s, true)
}

func EffectiveInboxRuleProvider(s string) string {
	if s == "" {
		return DefaultInboxRuleProvider
	}
	return s
}

func validRuleText(s string, ascii bool) bool {
	return len(s) > 0 && len(s) <= 120 && utf8.ValidString(s) && s == strings.TrimSpace(s) &&
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || ascii && r > unicode.MaxASCII }) == -1
}

type InboxRules struct {
	Revision string
	Rules    []InboxRule
}

// IncomingRuleMailbox must return ErrUnsupported for a script which cannot
// be completely projected. Replacement is conditional on the observed revision.
type IncomingRuleMailbox interface {
	ReadInboxRules(context.Context) (InboxRules, error)
	ReplaceInboxRules(context.Context, string, []InboxRule) error
}
