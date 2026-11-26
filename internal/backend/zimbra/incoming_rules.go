// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

const incomingRulesNS = "urn:ewsgo:zimbra:rules:1"

var _ core.IncomingRuleMailbox = (*mailbox)(nil)

func ruleMissing(err error) bool {
	var fault *Fault
	return errors.As(err, &fault) && fault.Code == "service.UNKNOWN_DOCUMENT"
}

func validRulesRevision(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func (m *mailbox) ReadInboxRules(ctx context.Context) (core.InboxRules, error) {
	out, err := m.native.request(ctx, incomingRulesNS, "ReadIncomingRules", "", element(incomingRulesNS, "ReadIncomingRulesRequest", ""))
	if ruleMissing(err) {
		return core.InboxRules{}, core.ErrUnsupported
	}
	if err != nil {
		return core.InboxRules{}, semanticError(err)
	}
	revision := out.get("revision")
	if !validRulesRevision(revision) || out.child(incomingRulesNS, "script") == nil {
		return core.InboxRules{}, ErrProtocol
	}
	projection := out.child(mailNS, "GetFilterRulesResponse")
	if projection == nil {
		return core.InboxRules{}, core.ErrUnsupported
	}
	container := projection.child(mailNS, "filterRules")
	if len(projection.Children) == 0 {
		return core.InboxRules{Revision: revision}, nil
	}
	if container == nil || len(projection.Children) != 1 || len(container.Children) > core.MaxInboxRules {
		return core.InboxRules{}, core.ErrUnsupported
	}
	list := make([]core.InboxRule, 0, len(container.Children))
	names := make(map[string]bool, len(container.Children))
	for _, item := range container.Children {
		rule, ok := projectInboxRule(item)
		if !ok || names[rule.Name] {
			return core.InboxRules{}, core.ErrUnsupported
		}
		names[rule.Name] = true
		list = append(list, rule)
	}
	return core.InboxRules{Revision: revision, Rules: list}, nil
}

// Match the structured XML precisely. Extra tests or actions in an existing
// script make the whole ruleset ineligible for a MAPI replacement.
func matchesRuleNode(n node, name string, attrs ...string) bool {
	if n.XMLName.Space != mailNS || n.XMLName.Local != name || strings.TrimSpace(n.Text) != "" {
		return false
	}
	for _, a := range n.Attrs {
		if a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns" {
			continue
		}
		if a.Name.Space != "" {
			return false
		}
		found := false
		for _, attr := range attrs {
			if attr == a.Name.Local {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func projectInboxRule(n node) (core.InboxRule, bool) {
	var rule core.InboxRule
	if !matchesRuleNode(n, "filterRule", "name", "active") || len(n.Children) != 2 {
		return rule, false
	}
	tests, actions := n.Children[0], n.Children[1]
	if !matchesRuleNode(tests, "filterTests", "condition") || tests.get("condition") != "allof" || len(tests.Children) != 1 ||
		!matchesRuleNode(actions, "filterActions") || len(actions.Children) < 1 || len(actions.Children) > 3 {
		return rule, false
	}
	test := tests.Children[0]
	rule.Name = n.get("name")
	if !matchesRuleNode(test, "headerTest", "header", "index", "value", "stringComparison") ||
		!strings.EqualFold(test.get("header"), "Subject") || test.get("index") != "0" ||
		test.get("stringComparison") != "contains" || len(test.Children) != 0 {
		return rule, false
	}
	var markRead bool
	for i, action := range actions.Children {
		if action.get("index") != strconv.Itoa(i) || len(action.Children) != 0 {
			return rule, false
		}
		switch {
		case matchesRuleNode(action, "actionFlag", "index", "flagName") && action.get("flagName") == "read" && i == 0:
			markRead = true
		case matchesRuleNode(action, "actionTag", "index", "tagName") && (i == 0 || i == 1 && markRead):
			name, definition, ok := strings.Cut(rule.Name, deferredRuleSuffix)
			wire, err := base64.RawURLEncoding.DecodeString(definition)
			id, provider, data, deferred, valid := parseDeferredDefinition(wire)
			if !ok || err != nil || !valid || deferredMarker(wire) != action.get("tagName") {
				return rule, false
			}
			rule.Name, rule.DeferredActions, rule.DeferredRuleID = name, deferred, id
			rule.Provider, rule.ProviderData = provider, data
			rule.MarkRead = markRead
		case matchesRuleNode(action, "actionStop", "index") && i > 0 && i == len(actions.Children)-1:
			rule.Stop = true
		default:
			return rule, false
		}
	}
	if len(rule.DeferredActions) == 0 {
		if name, suffix, hasMetadata := strings.Cut(rule.Name, nativeRuleSuffix); hasMetadata {
			wire, err := base64.RawURLEncoding.DecodeString(suffix)
			provider, data, remainder, valid := parseRuleProvider(wire)
			if err != nil || !valid || len(remainder) != 0 {
				return rule, false
			}
			rule.Name, rule.Provider, rule.ProviderData = name, provider, data
		}
	}
	rule.SubjectContains = test.get("value")
	rule.Enabled = n.get("active") == "1"
	return rule, rule.Valid() && (rule.Enabled || n.get("active") == "0")
}

func (m *mailbox) ReplaceInboxRules(ctx context.Context, revision string, list []core.InboxRule) error {
	if !validRulesRevision(revision) || len(list) > core.MaxInboxRules {
		return core.ErrInvalid
	}
	names := make(map[string]bool, len(list))
	rules := make([]node, 0, len(list))
	for _, rule := range list {
		if !rule.Valid() || names[rule.Name] || strings.Contains(rule.Name, deferredRuleSuffix) || strings.Contains(rule.Name, nativeRuleSuffix) {
			return core.ErrInvalid
		}
		names[rule.Name] = true
		active := "0"
		if rule.Enabled {
			active = "1"
		}
		subject := attr(attr(attr(attr(element(mailNS, "headerTest", ""), "index", "0"), "header", "Subject"), "stringComparison", "contains"), "value", rule.SubjectContains)
		tests := attr(element(mailNS, "filterTests", "", subject), "condition", "allof")
		name := rule.Name
		var metadata []byte
		if core.EffectiveInboxRuleProvider(rule.Provider) != core.DefaultInboxRuleProvider || len(rule.ProviderData) != 0 {
			metadata = appendRuleProvider(nil, rule.Provider, rule.ProviderData)
		}
		actions := element(mailNS, "filterActions", "")
		appendAction := func(action node) {
			actions.Children = append(actions.Children, attr(action, "index", strconv.Itoa(len(actions.Children))))
		}
		if len(rule.DeferredActions) == 0 || rule.MarkRead {
			appendAction(attr(element(mailNS, "actionFlag", ""), "flagName", "read"))
		}
		if len(rule.DeferredActions) != 0 {
			id := rule.DeferredRuleID
			if len(metadata) != 0 {
				id |= deferredProviderBit
			}
			wire := binary.BigEndian.AppendUint64(nil, id)
			wire = append(wire, metadata...)
			wire = append(wire, rule.DeferredActions...)
			name += deferredRuleSuffix + base64.RawURLEncoding.EncodeToString(wire)
			appendAction(attr(element(mailNS, "actionTag", ""), "tagName", deferredMarker(wire)))
		} else if len(metadata) != 0 {
			name += nativeRuleSuffix + base64.RawURLEncoding.EncodeToString(metadata)
		}
		if rule.Stop {
			appendAction(element(mailNS, "actionStop", ""))
		}
		rules = append(rules, attr(attr(element(mailNS, "filterRule", "", tests, actions), "name", name), "active", active))
	}
	native := element(mailNS, "ModifyFilterRulesRequest", "", element(mailNS, "filterRules", "", rules...))
	request := attr(element(incomingRulesNS, "ReplaceIncomingRulesRequest", "", native), "expected", revision)
	out, err := m.native.request(ctx, incomingRulesNS, "ReplaceIncomingRules", "", request)
	switch {
	case ruleMissing(err):
		return core.ErrUnsupported
	case errors.Is(err, ErrDenied), errors.Is(err, ErrExpired), errors.Is(err, ErrAuth):
		return semanticError(err)
	case err != nil:
		return core.ErrOutcomeUnknown
	}
	switch out.get("status") {
	case "applied":
		return nil
	case "conflict":
		return core.ErrConflict
	case "unsupported":
		return core.ErrUnsupported
	default:
		return core.ErrOutcomeUnknown
	}
}

const deferredRuleSuffix = " [ewsgo-dam-v1:"
const nativeRuleSuffix = " [ewsgo-rule-v1:"
const deferredProviderBit uint64 = 1 << 63

func appendRuleProvider(out []byte, provider string, data []byte) []byte {
	out = append(out, byte(len(provider)), byte(len(data)))
	out = append(out, provider...)
	return append(out, data...)
}

func parseRuleProvider(wire []byte) (string, []byte, []byte, bool) {
	if len(wire) < 2 {
		return "", nil, nil, false
	}
	p, d := 2+int(wire[0]), 2+int(wire[0])+int(wire[1])
	if d > len(wire) {
		return "", nil, nil, false
	}
	provider := string(wire[2:p])
	return provider, wire[p:d], wire[d:], core.ValidInboxRuleProvider(provider)
}

func parseDeferredDefinition(wire []byte) (id uint64, provider string, data, actions []byte, valid bool) {
	if len(wire) < 8 {
		return
	}
	id, actions = binary.BigEndian.Uint64(wire), wire[8:]
	if id&deferredProviderBit != 0 {
		id &^= deferredProviderBit
		provider, data, actions, valid = parseRuleProvider(actions)
		if !valid {
			return
		}
	}
	return id, provider, data, actions, id != 0 && core.ValidDeferredActions(actions)
}

func deferredMarker(definition []byte) string {
	digest := sha256.Sum256(definition)
	return "ewsgo-dam-v1-" + hex.EncodeToString(digest[:16])
}
