// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"

	"ewsgo/internal/core"
)

const (
	ropGetRulesTable = 0x3f
	folderHasRules   = 0x663a000b
	ruleID           = 0x66740014
	ruleSequence     = 0x66760003
	ruleState        = 0x66770003
	ruleUserFlags    = 0x66780003
	ruleCondition    = 0x667900fd
	ruleActions      = 0x668000fe
	ruleProvider     = 0x6681001f
	ruleName         = 0x6682001f
	ruleLevel        = 0x66830003
	ruleProviderData = 0x66840102
	ruleSubject      = 0x0037001f
	ruleEnabled      = 0x01
	ruleExitLevel    = 0x10
)

var markReadRuleAction = [...]byte{1, 0, 9, 0, 0x0b, 0, 0, 0, 0, 0, 0, 0, 0}

// The OpenFolder flag and PidTagHasRules describe the same rules table.
func inboxHasRules(ctx context.Context, session core.Session, binding storeBinding, fid uint64) (byte, error) {
	if fid == 0 || fid != binding.fids[Inbox] {
		return 0, nil
	}
	backend, ok := session.Mailbox().(core.IncomingRuleMailbox)
	if !ok {
		return 0, nil
	}
	snapshot, err := backend.ReadInboxRules(ctx)
	if err != nil {
		return 0, err
	}
	if len(snapshot.Rules) != 0 {
		return 1, nil
	}
	return 0, nil
}

func inboxRulesMailbox(ctx context.Context, session core.Session, binding storeBinding, fid uint64) (core.IncomingRuleMailbox, error) {
	if session == nil || session.Mailbox() == nil {
		return nil, core.ErrAccessDenied
	}
	backend, ok := session.Mailbox().(core.IncomingRuleMailbox)
	if !ok {
		return nil, core.ErrUnsupported
	}
	if fid != binding.fids[Inbox] {
		return nil, core.ErrUnsupported
	}
	if err := authorizeHierarchyTable(ctx, session, binding.store, fid); err != nil {
		return nil, err
	}
	return backend, nil
}

func inboxRuleID(binding storeBinding, name string) uint64 {
	digest := sha256.New()
	digest.Write([]byte("ewsgo/mapi/incoming-rule/1\x00"))
	digest.Write(binding.mailboxGUID[:])
	digest.Write([]byte(name))
	return binary.LittleEndian.Uint64(digest.Sum(nil)) & 0x7fffffffffffffff
}

func captureRulesTable(ctx context.Context, session core.Session, binding storeBinding, fid uint64, flags byte) (*contentsTable, error) {
	backend, err := inboxRulesMailbox(ctx, session, binding, fid)
	if err != nil {
		return nil, err
	}
	snapshot, err := backend.ReadInboxRules(ctx)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Rules) > core.MaxInboxRules {
		return nil, core.ErrUnsupported
	}
	lease, err := binding.store.reserveContentsPeak(ctx, 2048+len(snapshot.Rules)*3072)
	if err != nil {
		return nil, err
	}
	table := &contentsTable{inboxRules: true, flags: flags, lease: lease}
	seen := make(map[uint64]bool, len(snapshot.Rules))
	for i, rule := range snapshot.Rules {
		id := inboxRuleID(binding, rule.Name)
		if !rule.Valid() || id == 0 || seen[id] || rule.DeferredRuleID != 0 && rule.DeferredRuleID != id {
			table.close()
			return nil, core.ErrUnsupported
		}
		seen[id] = true
		state := 0
		if rule.Enabled {
			state |= ruleEnabled
		}
		if rule.Stop {
			state |= ruleExitLevel
		}
		restriction := append32([]byte{3}, 0x10001) // RES_CONTENT, FL_SUBSTRING | FL_IGNORECASE.
		restriction = append32(append32(restriction, ruleSubject), ruleSubject)
		subject, _ := propertyString(rule.SubjectContains, 0x1f, 0)
		row := make(contentsValues, 14)
		putTableRowIdentity(row, id)
		putContentsValue(row, ruleID, core.Integer(id))
		putContentsValue(row, ruleSequence, core.Integer(i+1))
		putContentsValue(row, ruleState, core.Integer(state))
		putContentsValue(row, ruleName, core.Text(rule.Name))
		putContentsValue(row, ruleProvider, core.Text(core.EffectiveInboxRuleProvider(rule.Provider)))
		putContentsValue(row, ruleCondition, core.Binary(append(restriction, subject...)))
		actions := markReadRuleAction[:]
		if len(rule.DeferredActions) != 0 {
			actions = rule.DeferredActions
			if rule.MarkRead {
				count := binary.LittleEndian.Uint16(actions)
				actions = binary.LittleEndian.AppendUint16(nil, count+1)
				actions = append(actions, markReadRuleAction[2:]...)
				actions = append(actions, rule.DeferredActions[2:]...)
			}
		}
		putContentsValue(row, ruleActions, core.Binary(slices.Clone(actions)))
		putContentsValue(row, ruleUserFlags, core.Integer(0))
		putContentsValue(row, ruleLevel, core.Integer(0))
		putContentsValue(row, ruleProviderData, core.Binary(slices.Clone(rule.ProviderData)))
		table.rows = append(table.rows, row)
	}
	return table, nil
}

type inboxRuleChange struct {
	flags byte
	props map[uint32][]byte
}

func parseInboxRuleChanges(wire []byte) (bool, []inboxRuleChange, error) {
	if len(wire) < 3 {
		return false, nil, core.ErrInvalid
	}
	if wire[0]&^byte(1) != 0 {
		return false, nil, core.ErrInvalid
	}
	count := int(binary.LittleEndian.Uint16(wire[1:3]))
	if count > core.MaxInboxRules {
		return false, nil, core.ErrUnsupported
	}
	replace := wire[0] == 1
	r := reader{data: wire, pos: 3}
	out := make([]inboxRuleChange, 0, count)
	for range count {
		header := r.take(3)
		if header == nil {
			return false, nil, core.ErrInvalid
		}
		flags, n := header[0], int(binary.LittleEndian.Uint16(header[1:]))
		if (flags != 1 && flags != 2 && flags != 4) || replace && flags != 1 || n == 0 || n > 10 {
			return false, nil, core.ErrInvalid
		}
		change := inboxRuleChange{flags: flags, props: make(map[uint32][]byte, n)}
		for range n {
			tag := r.u32()
			if _, exists := change.props[tag]; exists {
				return false, nil, core.ErrInvalid
			}
			start := r.pos
			if err := skipRuleProperty(&r, uint16(tag), 0); err != nil {
				return false, nil, core.ErrInvalid
			}
			change.props[tag] = wire[start:r.pos]
		}
		out = append(out, change)
	}
	if r.err != nil || r.pos != len(wire) {
		return false, nil, core.ErrInvalid
	}
	return replace, out, nil
}

type orderedInboxRule struct {
	rule core.InboxRule
	seq  int32
}

func applyInboxRuleChanges(binding storeBinding, rules []core.InboxRule, replace bool, changes []inboxRuleChange) ([]core.InboxRule, error) {
	if len(rules) > core.MaxInboxRules {
		return nil, core.ErrUnsupported
	}
	next := make([]orderedInboxRule, 0, len(rules))
	if !replace {
		for i, rule := range rules {
			next = append(next, orderedInboxRule{rule: rule, seq: int32(i + 1)})
		}
	}
	for _, change := range changes {
		if change.flags == 1 {
			if change.props[ruleID] != nil || change.props[ruleProvider] == nil ||
				change.props[ruleCondition] == nil || change.props[ruleActions] == nil {
				return nil, core.ErrInvalid
			}
			sequence := int32(1)
			for _, prior := range next {
				sequence = max(sequence, prior.seq+1)
			}
			rule := orderedInboxRule{rule: core.InboxRule{Enabled: true}, seq: sequence}
			if err := change.update(binding, &rule, true); err != nil {
				return nil, err
			}
			next = append(next, rule)
			continue
		}
		wire := change.props[ruleID]
		if len(wire) != 8 {
			return nil, core.ErrInvalid
		}
		id := binary.LittleEndian.Uint64(wire)
		i := slices.IndexFunc(next, func(rule orderedInboxRule) bool { return inboxRuleID(binding, rule.rule.Name) == id })
		if i < 0 {
			return nil, core.ErrNotFound
		}
		if change.flags == 4 {
			if len(change.props) != 1 {
				return nil, core.ErrInvalid
			}
			next = slices.Delete(next, i, i+1)
		} else if err := change.update(binding, &next[i], false); err != nil {
			return nil, err
		}
	}
	if len(next) > core.MaxInboxRules {
		return nil, core.ErrUnsupported
	}
	slices.SortStableFunc(next, func(a, b orderedInboxRule) int {
		if a.seq < b.seq {
			return -1
		}
		if a.seq > b.seq {
			return 1
		}
		return 0
	})
	out := make([]core.InboxRule, len(next))
	seen := make(map[uint64]bool, len(next))
	for i, row := range next {
		id := inboxRuleID(binding, row.rule.Name)
		if id == 0 || seen[id] || row.rule.DeferredRuleID != 0 && row.rule.DeferredRuleID != id {
			return nil, core.ErrInvalid
		}
		seen[id] = true
		out[i] = row.rule
	}
	return out, nil
}

func (c inboxRuleChange) update(binding storeBinding, entry *orderedInboxRule, adding bool) error {
	rule := &entry.rule
	for tag, value := range c.props {
		switch tag {
		case ruleID:
		case ruleSequence:
			entry.seq = int32(binary.LittleEndian.Uint32(value))
			if entry.seq < 0 || entry.seq > 0x3fffffff {
				return core.ErrUnsupported
			}
		case ruleState:
			flags := binary.LittleEndian.Uint32(value)
			if flags & ^uint32(ruleEnabled|ruleExitLevel) != 0 {
				return core.ErrUnsupported
			}
			rule.Enabled, rule.Stop = flags&ruleEnabled != 0, flags&ruleExitLevel != 0
		case ruleName, ruleProvider:
			r := reader{data: value}
			s, err := createFolderString(&r, 120)
			if err != nil || r.pos != len(value) {
				return core.ErrInvalid
			}
			if tag == ruleProvider {
				if s == "" || !core.ValidInboxRuleProvider(s) {
					return core.ErrUnsupported
				}
				rule.Provider = s
			} else {
				if !adding && s != rule.Name {
					return core.ErrUnsupported
				}
				rule.Name = s
			}
		case ruleCondition:
			r := reader{data: value}
			b := r.take(1)
			if len(b) != 1 || b[0] != 3 || r.u32() != 0x10001 || r.u32() != ruleSubject || r.u32() != ruleSubject {
				return core.ErrUnsupported
			}
			s, err := createFolderString(&r, 120)
			if err != nil || r.pos != len(value) {
				return core.ErrInvalid
			}
			rule.SubjectContains = s
		case ruleActions:
			rule.MarkRead = false
			switch {
			case bytes.Equal(value, markReadRuleAction[:]):
				rule.DeferredActions = nil
			case core.ValidDeferredActions(value):
				rule.DeferredActions = slices.Clone(value)
			case len(value) > len(markReadRuleAction) && bytes.HasPrefix(value[2:], markReadRuleAction[2:]):
				count := binary.LittleEndian.Uint16(value)
				if count < 2 {
					return core.ErrInvalid
				}
				deferred := append(binary.LittleEndian.AppendUint16(nil, count-1), value[len(markReadRuleAction):]...)
				if !core.ValidDeferredActions(deferred) {
					return core.ErrUnsupported
				}
				rule.MarkRead, rule.DeferredActions = true, deferred
			default:
				return core.ErrUnsupported
			}
		case ruleUserFlags, ruleLevel:
			if binary.LittleEndian.Uint32(value) != 0 {
				return core.ErrUnsupported
			}
		case ruleProviderData:
			if len(value)-2 > core.MaxInboxRuleProviderDataBytes {
				return core.ErrUnsupported
			}
			rule.ProviderData = slices.Clone(value[2:])
		default:
			return core.ErrUnsupported
		}
	}
	rule.DeferredRuleID = 0
	if len(rule.DeferredActions) > 0 {
		rule.DeferredRuleID = inboxRuleID(binding, rule.Name)
	}
	if !rule.Valid() {
		return core.ErrInvalid
	}
	return nil
}

func modifyInboxRules(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.isLogon || object.download != nil {
		return fail(NotSupported)
	}
	backend, err := inboxRulesMailbox(ctx, session, object.binding, object.fid)
	if err != nil {
		return fail(storeError(err))
	}
	replace, changes, err := parseInboxRuleChanges(op.RuleChanges)
	if err != nil {
		return fail(storeError(err))
	}
	before, err := backend.ReadInboxRules(ctx)
	if err != nil {
		return fail(storeError(err))
	}
	after, err := applyInboxRuleChanges(object.binding, before.Rules, replace, changes)
	if err != nil {
		return fail(storeError(err))
	}
	err = backend.ReplaceInboxRules(ctx, before.Revision, after)
	if errors.Is(err, core.ErrConflict) {
		return fail(0x80040109) // ecObjectModified.
	}
	return fail(storeErrorOrSuccess(err))
}
