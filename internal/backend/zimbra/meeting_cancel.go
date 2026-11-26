// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) StageMeetingCancellation(ctx context.Context, before core.Appointment, cancellation core.MeetingCancellation) (core.SendAttempt, error) {
	o := before.Identity
	id, ok := strings.CutPrefix(string(o.ID), core.AppointmentIDPrefix)
	if !ok || !nativeID.MatchString(id) || o.Kind != core.AppointmentItem || o.Revision == 0 ||
		before.Type != core.AppointmentSingle || before.Cancelled {
		return nil, core.ErrUnsupported
	}
	detail, err := m.readAppointment(ctx, o.Parent, "id", id, false)
	if err != nil {
		return nil, err
	}
	current, err := appointmentIdentity(*detail, o.Parent)
	if err != nil {
		return nil, err
	}
	if current.Revision != o.Revision || current.Properties[core.ContentRevision] != o.Properties[core.ContentRevision] ||
		detail.get("uid") != before.UID {
		return nil, core.ErrConflict
	}
	invite, part := appointmentInvite(*detail, func(candidate node) bool { return candidate.get("ex") != "1" })
	if part == nil || part.get("method") != "REQUEST" || part.get("neverSent") == "1" ||
		part.child(mailNS, "recur") != nil {
		return nil, core.ErrUnsupported
	}
	recipients, err := m.meetingRecipients(*part)
	if err != nil {
		return nil, err
	}
	invID, compNum, err := appointmentInviteAddress(invite)
	if err != nil {
		return nil, err
	}
	req := attr(attr(element(mailNS, "CancelAppointmentRequest", ""), "id", id+"-"+invID), "comp", compNum)
	if cancellation.Body != "" {
		contentType := "text/plain"
		if cancellation.HTML {
			contentType = "text/html"
		}
		body := attr(element(mailNS, "mp", "", element(mailNS, "content", cancellation.Body)), "ct", contentType)
		children := append(recipients, element(mailNS, "su", "Canceled: "+part.get("name")), body)
		req.Children = []node{element(mailNS, "m", "", children...)}
	}
	return func(ctx context.Context) (core.ID, error) {
		_, err := m.native.request(ctx, mailNS, "CancelAppointment", "", req, modifiedConstraint(o.Revision))
		if err != nil {
			var fault *Fault
			if errors.As(err, &fault) && fault.Code == "mail.INVITE_OUT_OF_DATE" {
				return "", core.ErrSendRejected
			}
			return "", core.ErrOutcomeUnknown
		}
		return o.ID, nil
	}, nil
}
