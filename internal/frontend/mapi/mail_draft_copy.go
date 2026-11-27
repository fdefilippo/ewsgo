// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"maps"
	"mime"
	"net/textproto"
	"slices"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const (
	ropCopyTo         byte = 0x39
	ropCopyProperties byte = 0x67
)

type copyMessageToRequest struct {
	destination byte
	subobjects  bool
	flags       byte
	excluded    []uint32
	included    []uint32
}

func parseCopyMessageTo(r *reader, out *copyMessageToRequest, handles int, include bool) error {
	flagOffset := 3
	if include {
		flagOffset = 2
	}
	h := r.take(flagOffset + 3)
	if r.err != nil || int(h[0]) >= handles {
		return ErrMalformed
	}
	out.destination, out.subobjects, out.flags = h[0], include || h[2] != 0, h[flagOffset]
	count := int(binary.LittleEndian.Uint16(h[flagOffset+1:]))
	if count > (len(r.data)-r.pos)/4 {
		return ErrMalformed
	}
	tags := make([]uint32, count)
	for i := range tags {
		tags[i] = r.u32()
	}
	if include {
		out.included = tags
	} else {
		out.excluded = tags
	}
	return r.err
}

func copyMessageTo(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	request := op.CopyMessageTo
	if request.flags & ^byte(3) != 0 {
		return fail(0x80070057)
	}
	source, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	target, code := objects.lookup(handles[request.destination], op.Logon)
	if code == nullObject || code == invalidObject {
		return append32(fail(0x00000503), uint32(request.destination))
	}
	if code != 0 {
		return fail(code)
	}
	if request.included != nil && (target.message == nil) != (source.message == nil) {
		return fail(0x80040306) // MAPI_E_DECLINE_COPY.
	}
	if target.binding != source.binding || target.message == nil || source.message == nil ||
		target.message.mailDraft == nil || source.message.mailDraft == nil {
		return fail(NotSupported)
	}
	dst, src := target.message.mailDraft, source.message.mailDraft
	if dst.mid == src.mid {
		return fail(0x80070057)
	}
	problems, err := dst.copyFrom(ctx, session, src, request)
	if err != nil {
		return fail(faiError(err, false))
	}
	return propertyProblemsResponse(op.ID, op.Input, problems)
}

// Copy only the writable projection of the two authenticated normal drafts.
// Identity, revisions, placement and the native draft reference belong to the
// destination. Move permits retaining the source (MS-OXCPRPT 3.2.5.8).
func (d *mailDraft) copyFrom(ctx context.Context, session core.Session, source *mailDraft, request copyMessageToRequest) ([]faiPropertyProblem, error) {
	if request.included == nil && (d.incompleteAttachments != 0 || request.subobjects && source.incompleteAttachments != 0) {
		return nil, core.ErrInvalid
	}
	if err := d.editable(); err != nil {
		return nil, err
	}
	if err := d.authorize(ctx, session); err != nil {
		return nil, err
	}
	if err := source.authorize(ctx, session); err != nil {
		return nil, err
	}
	lease, err := d.store.reserveContentsPeak(ctx, 6*core.MaxMailContentMIMEBytes)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	src, err := mailmime.ReadDraft(source.staged.MIME)
	if err != nil {
		return nil, err
	}
	dst, err := mailmime.ReadDraft(d.staged.MIME)
	if err != nil {
		return nil, err
	}
	force := request.flags&2 == 0
	has := func(tag uint32) bool { return !slices.Contains(request.excluded, tag) }
	srcBody, srcFiles, err := splitDraftCopyParts(src)
	if err != nil {
		return nil, err
	}
	dstBody, dstFiles, err := splitDraftCopyParts(dst)
	if err != nil {
		return nil, err
	}
	var problems []faiPropertyProblem
	if request.included != nil {
		srcProps := draftCopyProperties(src, srcBody, len(source.rows), len(srcFiles))
		dstProps := draftCopyProperties(dst, dstBody, len(d.rows), len(dstFiles))
		canCopyFiles := source.incompleteAttachments == 0 && d.incompleteAttachments == 0 && len(dstFiles) == 0
		selected, failed := selectDraftCopyTags(request.included, force, srcProps, dstProps, canCopyFiles)
		problems = failed
		if len(selected) == 0 {
			return problems, nil
		}
		has = func(tag uint32) bool { return selected[tag] }
		force = true // NoOverwrite has already filtered the selected tags.
	}
	subject := d.staged.Properties[core.Subject]
	if value := src.Header.Get("Subject"); has(0x0037001f) && (request.included != nil || value != "") && (force || dst.Header.Get("Subject") == "") {
		dst.Header.Set("Subject", value)
		subject = source.staged.Properties[core.Subject]
	}
	copyRecipients := request.subobjects && has(0x0e12000d) && len(source.rows) != 0 && (force || len(d.rows) == 0)
	if copyRecipients {
		for _, name := range []string{"To", "Cc", "Bcc"} {
			dst.Header.Del(name)
			for _, value := range src.Header.Values(name) {
				dst.Header.Add(name, value)
			}
		}
	}
	copyFiles := request.subobjects && has(0x0e13000d) && len(srcFiles) != 0 && (force || len(dstFiles) == 0)
	if copyFiles && len(dstFiles) != 0 {
		// A live attachment handle addresses its original MIME part index.
		return nil, core.ErrUnsupported
	}
	copyPlain, copyHTML := has(0x1000001f), has(0x10130102)
	if request.included == nil || copyPlain || copyHTML || copyFiles {
		dstBody, err = copyDraftBody(srcBody, dstBody, force, copyPlain, copyHTML, request.included != nil)
		if err != nil {
			return nil, err
		}
		if copyFiles {
			dstFiles = srcFiles
		}
		dst.InstallBody(dstBody)
		if len(dstFiles) != 0 {
			if err := dst.AppendFiles(dstFiles); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.installMIME(dst); err != nil {
		return nil, err
	}
	if subject != nil {
		d.staged.Properties[core.Subject] = subject
	}
	if request.included == nil && has(0x001a001f) && (force || d.meeting.Class == "") && source.meeting.Class != "" {
		d.meeting = source.meeting
	}
	if copyRecipients {
		d.rows = maps.Clone(source.rows)
		d.staged.Recipients = slices.Clone(source.staged.Recipients)
	}
	if copyFiles {
		d.attachmentNumbers = make([]uint32, len(dstFiles))
		for i := range dstFiles {
			d.attachmentNumbers[i] = d.nextAttachment
			d.nextAttachment++
		}
		d.staged.Properties[core.HasAttachments] = core.Bool(true)
	}
	if request.included == nil && d.outbox && d.staged.ID == "" && !source.outbox && source.folder == d.folder &&
		validSavedMailDraft(source.staged, d.folder) {
		d.store.mu.Lock()
		if pending, ok := d.store.pendingMailDrafts[d.ref]; ok && !pending.dispatched {
			pending.transportSource = source.staged.ID
			d.store.pendingMailDrafts[d.ref] = pending
		}
		d.store.mu.Unlock()
	}
	return problems, nil
}

func draftCopyProperties(doc, body mailmime.Draft, recipients, files int) map[uint32]uint32 {
	status := func(exists bool) uint32 {
		if exists {
			return 0
		}
		return PropertyNotFound
	}
	plain, html, err := draftBodyVariants(body)
	_, subject := doc.Header["Subject"]
	props := map[uint32]uint32{
		0x0037001f: status(subject),
		0x1000001f: status(plain.Header != nil),
		0x10130102: status(html.Header != nil),
		0x0e12000d: status(recipients != 0),
		0x0e13000d: status(files != 0),
	}
	if err != nil {
		props[0x1000001f], props[0x10130102] = NotSupported, NotSupported
	}
	return props
}

func selectDraftCopyTags(tags []uint32, overwrite bool, source, target map[uint32]uint32, canCopyFiles bool) (map[uint32]bool, []faiPropertyProblem) {
	selected := make(map[uint32]bool, len(source))
	var problems []faiPropertyProblem
	for i, tag := range tags {
		valueTag := tag
		switch tag {
		case 0x0037001e, 0x1000001e: // String8 views of the same MIME fields.
			valueTag++
		}
		code, supported := source[valueTag]
		switch {
		case !supported:
			code = NotSupported
			switch uint16(tag >> 16) {
			case 0x0fff, 0x0ff9, 0x65e0, 0x65e1, 0x65e2, 0x65e3, 0x6748, 0x674a:
				code = accessDenied // Server-owned identity.
			}
		case !overwrite && target[valueTag] == 0:
			continue
		case target[valueTag] == NotSupported || valueTag == 0x0e13000d && !canCopyFiles:
			code = NotSupported
		}
		if code != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(i), tag: tag, code: code})
			continue
		}
		selected[valueTag] = true
	}
	return selected, problems
}

func splitDraftCopyParts(doc mailmime.Draft) (mailmime.Draft, []mailmime.Draft, error) {
	kind, _, err := mime.ParseMediaType(doc.Header.Get("Content-Type"))
	if err != nil {
		return mailmime.Draft{}, nil, core.ErrUnsupported
	}
	if kind != "multipart/mixed" {
		content := mailmime.Draft{Header: make(textproto.MIMEHeader)}
		content.InstallBody(doc)
		return content, nil, nil
	}
	parts, err := doc.Parts()
	if err != nil || len(parts) == 0 {
		return mailmime.Draft{}, nil, core.ErrUnsupported
	}
	return parts[0], parts[1:], nil
}

func draftBodyVariants(doc mailmime.Draft) (plain, html mailmime.Draft, err error) {
	kind, _, err := mime.ParseMediaType(doc.Header.Get("Content-Type"))
	if err != nil {
		return plain, html, core.ErrUnsupported
	}
	switch kind {
	case "text/plain":
		plain = doc
	case "text/html":
		html = doc
	case "multipart/alternative":
		parts, err := doc.Parts()
		if err != nil || len(parts) != 2 {
			return plain, html, core.ErrUnsupported
		}
		first, _, e1 := mime.ParseMediaType(parts[0].Header.Get("Content-Type"))
		second, _, e2 := mime.ParseMediaType(parts[1].Header.Get("Content-Type"))
		if e1 != nil || e2 != nil || first != "text/plain" || second != "text/html" {
			return plain, html, core.ErrUnsupported
		}
		plain, html = parts[0], parts[1]
	default:
		return plain, html, core.ErrUnsupported
	}
	return plain, html, nil
}

func copyDraftBody(src, dst mailmime.Draft, force, copyPlain, copyHTML, keepEmptyPlain bool) (mailmime.Draft, error) {
	if !copyPlain && !copyHTML {
		return dst, nil
	}
	srcPlain, srcHTML, err := draftBodyVariants(src)
	if err != nil {
		return mailmime.Draft{}, err
	}
	plain, html, err := draftBodyVariants(dst)
	if err != nil {
		return mailmime.Draft{}, err
	}
	if copyPlain && srcPlain.Header != nil && (force || len(plain.Body) == 0) {
		plain = srcPlain
	}
	if copyHTML && srcHTML.Header != nil && (force || html.Header == nil) {
		html = srcHTML
	}
	if html.Header == nil {
		return plain, nil
	}
	if plain.Header == nil || !keepEmptyPlain && len(plain.Body) == 0 {
		return html, nil
	}
	body, err := mailmime.Text("", false)
	if err != nil {
		return mailmime.Draft{}, err
	}
	err = body.SetMultipart("multipart/alternative", nil, []mailmime.Draft{plain, html})
	return body, err
}
