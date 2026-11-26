// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/core"
)

var _ core.MailboxWatcher = (*accountSession)(nil)

type mailboxWatch struct {
	session *Session
	account string
	id      string
	seq     string
}

func (s *accountSession) OpenMailboxWatch(ctx context.Context) (core.MailboxWatch, error) {
	account := s.identity.Scope.Account
	add := element(mailNS, "add", "", attr(element(mailNS, "a", ""), "id", account))
	request := attr(element(mailNS, "CreateWaitSetRequest", "", add), "defTypes", "f,m")
	response, err := s.native.request(ctx, mailNS, "CreateWaitSet", "", request)
	if err != nil {
		return nil, semanticError(err)
	}
	watch := &mailboxWatch{session: s.native, account: account, id: response.get("waitSet"), seq: response.get("seq")}
	if !validWaitSetValue(watch.id) {
		return nil, ErrProtocol
	}
	if !validWaitSetValue(watch.seq) || len(response.Children) != 0 {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = watch.Close(cleanup)
		return nil, ErrProtocol
	}
	return watch, nil
}

func (w *mailboxWatch) Wait(ctx context.Context) (bool, error) {
	if w.id == "" {
		return false, core.ErrInvalid
	}
	request := attr(attr(element(mailNS, "WaitSetRequest", ""), "waitSet", w.id), "seq", w.seq)
	request = attr(attr(request, "block", "1"), "timeout", "30")
	response, err := w.session.request(ctx, mailNS, "WaitSet", "", request)
	if err != nil {
		return false, waitSetError(err)
	}
	if response.get("waitSet") != w.id {
		return false, ErrProtocol
	}
	if response.get("canceled") == "1" || response.get("canceled") == "true" {
		return false, core.ErrConflict
	}
	seq := response.get("seq")
	if !validWaitSetValue(seq) || len(response.Children) > 1 {
		return false, ErrProtocol
	}
	if len(response.Children) == 1 {
		notice := response.Children[0]
		// Zimbra 10.1 emits a; the older WaitSet protocol note shows n.
		if notice.XMLName.Space != mailNS || (notice.XMLName.Local != "a" && notice.XMLName.Local != "n") || notice.get("id") != w.account {
			return false, ErrProtocol
		}
	}
	w.seq = seq
	return len(response.Children) == 1, nil
}

func (w *mailboxWatch) Close(ctx context.Context) error {
	if w.id == "" {
		return nil
	}
	request := attr(element(mailNS, "DestroyWaitSetRequest", ""), "waitSet", w.id)
	_, err := w.session.request(ctx, mailNS, "DestroyWaitSet", "", request)
	w.id = ""
	err = waitSetError(err)
	if errors.Is(err, core.ErrResync) {
		return nil // Already removed upstream.
	}
	return err
}

func validWaitSetValue(value string) bool {
	// Sequence values are opaque. Accept visible ASCII without interpreting it.
	if len(value) == 0 || len(value) > 512 {
		return false
	}
	for _, b := range []byte(value) {
		if b <= ' ' || b >= 0x7f {
			return false
		}
	}
	return true
}

func waitSetError(err error) error {
	var fault *Fault
	if errors.As(err, &fault) && (fault.Code == "admin.NO_SUCH_WAITSET" || fault.Code == "mail.NO_SUCH_WAITSET") {
		return core.ErrResync
	}
	return semanticError(err)
}
