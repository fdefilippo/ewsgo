// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"ewsgo/internal/core"
)

var _ core.MailMarksWriter = (*mailbox)(nil)

func (m *mailbox) SetMailMarks(ctx context.Context, id core.ID, expected core.Revision, marks core.MailMarks) (_ core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "SetMailMarks", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, ok := parseDeltaObject(id)
	if !ok || source.kind != core.Message || expected > math.MaxInt32 ||
		marks.Importance != nil && (*marks.Importance < 0 || *marks.Importance > 2) || marks.FollowUp != nil && !marks.FollowUp.Valid() {
		return core.Object{}, core.ErrInvalid
	}
	if err := validateMarkCategories(marks.Categories); err != nil {
		return core.Object{}, err
	}
	item, err := m.native.getMessageNode(ctx, source.ref, "8192")
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	current, err := messageSummary(item)
	if err != nil {
		return core.Object{}, err
	}
	if !hasMessageRevision(current, expected) {
		return core.Object{}, core.ErrConflict
	}

	advance := func(next core.Object, err error) error {
		if err == nil && (next.Parent != current.Parent || next.Properties[core.ContentRevision] != current.Properties[core.ContentRevision]) {
			err = core.ErrOutcomeUnknown
		}
		if err != nil {
			if current.Revision != expected {
				return core.ErrOutcomeUnknown
			}
			return err
		}
		current = next
		return nil
	}
	metadata := ewsgoMessageMetadata(item)
	if next := applyMessageMetadata(current, metadata, marks); !maps.Equal(metadata, next) {
		if err := advance(m.writeMessageMetadata(ctx, source.ref, expected, next)); err != nil {
			return core.Object{}, err
		}
	}
	var marked *bool
	if marks.FollowUp != nil {
		applyMark := marks.FollowUp.Status == core.FollowUpMarked
		marked = &applyMark
	}
	for _, update := range []struct {
		flag  core.MailFlag
		value *bool
	}{
		{core.MailFlagRead, marks.Read},
		{core.MailFlagMarked, marked},
	} {
		if update.value != nil {
			err := advance(m.SetMailFlag(ctx, id, current.Revision, update.flag, *update.value))
			if err != nil {
				return core.Object{}, err
			}
		}
	}
	if marks.Categories == nil {
		if !matchesMailMarks(current, marks) {
			return core.Object{}, core.ErrOutcomeUnknown
		}
		return current, nil
	}
	oldCategories, ok := current.Properties[core.MailCategories].(core.Strings)
	if !ok {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	for _, group := range []struct {
		names, other core.Strings
		present      bool
	}{{oldCategories, marks.Categories, false}, {marks.Categories, oldCategories, true}} {
		for _, name := range group.names {
			if containsCategory(group.other, name) {
				continue
			}
			err := advance(m.setMailCategory(ctx, source.ref, current.Revision, name, group.present))
			if err != nil {
				return core.Object{}, err
			}
		}
	}
	if !matchesMailMarks(current, marks) {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return current, nil
}

func (m *mailbox) setMailCategory(ctx context.Context, ref string, revision core.Revision, name string, present bool) (core.Object, error) {
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	op := "!tag"
	if present {
		op = "tag"
	}
	action := attr(attr(attr(element(mailNS, "action", ""), "id", ref), "op", op), "tn", name)
	response, err := m.native.request(ctx, mailNS, "MsgAction", "", element(mailNS, "MsgActionRequest", "", action), modifiedConstraint(revision))
	if err != nil {
		return core.Object{}, messageWriteError(err)
	}
	ack := response.child(mailNS, "action")
	if len(response.Children) != 1 || ack == nil || ack.get("op") != op || ack.get("id") != ref {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	after, err := m.getMessageSummary(ctx, ref)
	categories, known := after.Properties[core.MailCategories].(core.Strings)
	if err != nil || after.Revision <= revision || !known || containsCategory(categories, name) != present {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return after, nil
}

func validateMarkCategories(names core.Strings) error {
	if len(names) > 50 {
		return core.ErrInvalid
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		canonical := strings.ToLower(name)
		if name == "" || len(name) > 128 || strings.TrimSpace(name) != name ||
			strings.ContainsAny(name, ":\\") || strings.ContainsFunc(name, unicode.IsControl) || seen[canonical] {
			return core.ErrInvalid
		}
		seen[canonical] = true
	}
	return nil
}

func containsCategory(names core.Strings, desired string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return strings.EqualFold(name, desired) })
}

func matchesMailMarks(object core.Object, marks core.MailMarks) bool {
	if marks.Read != nil && object.Properties[core.IsRead] != core.Bool(*marks.Read) ||
		marks.Importance != nil && object.Properties[core.MessageImportance] != *marks.Importance {
		return false
	}
	if flag := marks.FollowUp; flag != nil {
		actual := core.MailFollowUpOf(object.Properties)
		if actual.Status != flag.Status || !actual.Start.Equal(flag.Start) || !actual.Due.Equal(flag.Due) || !actual.Complete.Equal(flag.Complete) {
			return false
		}
	}
	if marks.Categories == nil {
		return true
	}
	actual, ok := object.Properties[core.MailCategories].(core.Strings)
	if !ok || len(actual) != len(marks.Categories) {
		return false
	}
	for _, expected := range marks.Categories {
		if !containsCategory(actual, expected) {
			return false
		}
	}
	return true
}
