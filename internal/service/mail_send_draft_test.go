// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"errors"
	"testing"

	"ewsgo/internal/core"
)

func TestDraftSubmissionSenderHeaders(t *testing.T) {
	const actor = "owner@example.invalid"
	for _, tt := range []struct {
		name, headers string
		want          error
	}{
		{"valid", "from: Mailbox Owner <" + actor + ">\r\nSender: " + actor, nil},
		{"missing", "Subject: Example", core.ErrAccessDenied},
		{"duplicate", "From: " + actor + "\r\nfrom: " + actor, core.ErrAccessDenied},
		{"different sender", "From: " + actor + "\r\nSender: other@example.invalid", core.ErrAccessDenied},
		{"resent", "From: " + actor + "\r\nresent-date: Thu, 03 Dec 2026 10:30:00 +0000", core.ErrUnsupported},
		{"invalid field name", "From: " + actor + "\r\nExample Field: value", core.ErrInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.headers + "\r\nTo: recipient@example.invalid\r\nMessage-ID: <fixture@example.invalid>\r\n\r\nBody")
			id, err := validateDraftSubmission(raw, actor)
			if !errors.Is(err, tt.want) || err == nil && id != "fixture@example.invalid" {
				t.Fatalf("submission returned %q, %v; expected %v", id, err, tt.want)
			}
		})
	}
}
