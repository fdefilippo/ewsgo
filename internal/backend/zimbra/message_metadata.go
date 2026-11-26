// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	"ewsgo/internal/core"
)

const ewsgoMetadataSection = "ewsgo"

func ewsgoMessageMetadata(item node) map[string]string {
	data := make(map[string]string)
	for _, meta := range item.Children {
		if meta.XMLName.Space != mailNS || meta.XMLName.Local != "meta" || meta.get("section") != ewsgoMetadataSection {
			continue
		}
		for _, field := range meta.Children {
			if name := field.get("n"); field.XMLName.Space == mailNS && field.XMLName.Local == "a" && name != "" {
				data[name] = field.Text
			}
		}
	}
	return data
}

func projectMessageMetadata(object core.Object, metadata map[string]string) {
	if n, err := strconv.ParseInt(metadata["importance"], 10, 8); err == nil && n >= 0 && n <= 2 {
		object.Properties[core.MessageImportance] = core.Integer(n)
	}
	if metadata["completed"] == "true" {
		object.Properties[core.FollowUpCompleted] = core.Bool(true)
	}
	for _, date := range []struct {
		name string
		key  core.Key
	}{{"start", core.FollowUpStart}, {"due", core.FollowUpDue}, {"completedAt", core.FollowUpCompleteAt}} {
		if value, err := time.Parse(time.RFC3339Nano, metadata[date.name]); err == nil && !value.IsZero() {
			object.Properties[date.key] = core.Timestamp(value.UTC())
		}
	}
}

func applyMessageMetadata(current core.Object, metadata map[string]string, patch core.MailMarks) map[string]string {
	updated := maps.Clone(metadata)
	if patch.Importance != nil && current.Properties[core.MessageImportance] != *patch.Importance {
		updated["importance"] = strconv.FormatInt(int64(*patch.Importance), 10)
	}
	if flag := patch.FollowUp; flag != nil {
		for _, name := range []string{"completed", "start", "due", "completedAt"} {
			delete(updated, name)
		}
		if flag.Status == core.FollowUpComplete {
			updated["completed"] = "true"
		}
		for _, date := range []struct {
			name  string
			value time.Time
		}{{"start", flag.Start}, {"due", flag.Due}, {"completedAt", flag.Complete}} {
			if !date.value.IsZero() {
				updated[date.name] = date.value.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return updated
}

func (m *mailbox) writeMessageMetadata(ctx context.Context, ref string, expected core.Revision, data map[string]string) (core.Object, error) {
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	var fields []node
	for _, name := range slices.Sorted(maps.Keys(data)) {
		fields = append(fields, attr(element(mailNS, "a", data[name]), "n", name))
	}
	meta := attr(element(mailNS, "meta", "", fields...), "section", ewsgoMetadataSection)
	request := attr(element(mailNS, "SetCustomMetadataRequest", "", meta), "id", ref)
	response, err := m.native.request(ctx, mailNS, "SetCustomMetadata", "", request, modifiedConstraint(expected))
	if err != nil {
		return core.Object{}, messageWriteError(err)
	}
	if response.get("id") != ref {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	item, err := m.native.getMessageNode(ctx, ref, "8192")
	if err != nil || !maps.Equal(ewsgoMessageMetadata(item), data) {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	after, err := messageSummary(item)
	if err != nil || after.Revision <= expected {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return after, nil
}
