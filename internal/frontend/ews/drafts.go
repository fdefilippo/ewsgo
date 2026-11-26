// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const publicStrings = "00020329-0000-0000-c000-000000000046"

func namedStringKey(uri node) (core.Key, error) {
	if !isElement(uri, types, "ExtendedFieldURI") || len(uri.Children) != 0 || uri.attr("PropertyType") != "String" ||
		uri.attr("PropertyTag") != "" || uri.attr("PropertyId") != "" {
		return core.Key{}, core.ErrUnsupported
	}
	set := strings.ToLower(uri.attr("PropertySetId"))
	if namespace := uri.attr("DistinguishedPropertySetId"); namespace != "" {
		if namespace != "PublicStrings" || set != "" {
			return core.Key{}, core.ErrUnsupported
		}
		set = publicStrings
	}
	key := core.Key{Set: set, Name: uri.attr("PropertyName")}
	if key.Set == "" || key.Name == "" || len(key.Name) > 255 || !key.Valid() {
		return core.Key{}, core.ErrInvalid
	}
	return key, nil
}

func readNamedString(property node) (core.Key, core.Text, error) {
	uri, text := property.child(types, "ExtendedFieldURI"), property.child(types, "Value")
	if uri == nil || text == nil || len(property.Children) != 2 || len(text.Children) != 0 || len(text.Text) > 4096 {
		return core.Key{}, "", core.ErrInvalid
	}
	key, err := namedStringKey(*uri)
	return key, core.Text(text.Text), err
}

func (h *Handler) createItems(ctx context.Context, op node, session core.Session) []node {
	const operation = "CreateItem"
	if containsItemType(op.child(messages, "Items"), "Contact") {
		return h.createContacts(ctx, op, session)
	}
	if containsItemType(op.child(messages, "Items"), "CalendarItem") {
		return h.createAppointments(ctx, op, session)
	}
	if op.attr("MessageDisposition") != "SaveOnly" {
		return syncError(operation, core.ErrUnsupported)
	}
	writer, ok := session.Mailbox().(core.MailDraftWriter)
	if !ok {
		return syncError(operation, core.ErrUnsupported)
	}
	list := op.child(messages, "Items")
	savedTo := op.child(messages, "SavedItemFolderId")
	parts := 1
	if savedTo != nil {
		parts++
	}
	if !validBatch(list) || len(op.Children) != parts {
		return syncError(operation, core.ErrInvalid)
	}
	drafts, err := h.folderID(ctx, session, at(el(types, "DistinguishedFolderId", ""), "Id", "drafts"))
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	if savedTo != nil {
		parent, err := h.writeTarget(ctx, session, savedTo)
		if err != nil {
			return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
		}
		if parent != drafts {
			return syncError(operation, core.ErrUnsupported)
		}
	}
	out := make([]node, 0, len(list.Children))
	for _, message := range list.Children {
		item, err := h.createDraft(ctx, session, writer, message)
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		out = append(out, success(operation, el(messages, "Items", "", h.message(session.Identity().Scope, item))))
	}
	return out
}

func (h *Handler) createDraft(ctx context.Context, session core.Session, writer core.MailDraftWriter, message node) (core.Object, error) {
	if !isElement(message, types, "Message") {
		return core.Object{}, core.ErrUnsupported
	}
	doc, err := mailmime.NewDraft(session.Identity().Name)
	if err != nil {
		return core.Object{}, err
	}
	fields := make(map[core.Key]core.Text)
	var marks core.MailMarks
	var files []draftMIME
	seen := make(map[string]bool)
	for _, field := range message.Children {
		name := field.XMLName.Local
		if field.XMLName.Space != types || seen[name] && name != "ExtendedProperty" {
			return core.Object{}, core.ErrInvalid
		}
		seen[name] = true
		switch name {
		case "ExtendedProperty":
			key, text, err := readNamedString(field)
			if err != nil {
				return core.Object{}, err
			}
			if _, exists := fields[key]; exists || len(fields) == 32 {
				return core.Object{}, core.ErrInvalid
			}
			fields[key] = text
		case "Attachments":
			files, err = readFileAttachments(&field)
			if err != nil {
				return core.Object{}, err
			}
		case "Flag", "Categories", "IsRead":
			if err := setMailMarks(&marks, name, field); err != nil {
				return core.Object{}, err
			}
		default:
			if err := setDraftField(&doc, field); err != nil {
				return core.Object{}, err
			}
		}
	}
	if !seen["Body"] {
		empty, err := mailmime.Text("", false)
		if err != nil {
			return core.Object{}, err
		}
		doc.InstallBody(empty)
	}
	if len(files) != 0 {
		if err := doc.AppendFiles(files); err != nil {
			return core.Object{}, err
		}
	}
	var markWriter core.MailMarksWriter
	if !marks.Empty() {
		var ok bool
		markWriter, ok = session.Mailbox().(core.MailMarksWriter)
		if !ok {
			return core.Object{}, core.ErrUnsupported
		}
	}
	var mailboxID, ref string
	if len(fields) != 0 {
		if h.journal == nil {
			return core.Object{}, core.ErrUnsupported
		}
		if _, ok := session.Mailbox().(core.MailDraftReferenceFinder); !ok {
			return core.Object{}, core.ErrUnsupported
		}
		binding, err := h.journal.store.BindMailbox(ctx, session.Identity().Scope, h.journal.backendInstance)
		if err != nil {
			return core.Object{}, err
		}
		mailboxID = binding.MailboxID
		ref, err = h.journal.store.RegisterDraftProperties(ctx, mailboxID, fields)
		if err != nil {
			return core.Object{}, err
		}
		doc.Header.Set(core.DraftReferenceHeader, ref)
	}
	mime, err := doc.Bytes()
	if err != nil {
		h.discardDraftReference(ctx, mailboxID, ref)
		return core.Object{}, err
	}
	item, err := writer.SaveMailDraft(ctx, "", 0, mime)
	if err != nil {
		if errors.Is(err, core.ErrInvalid) || errors.Is(err, core.ErrAccessDenied) || errors.Is(err, core.ErrAuthentication) ||
			errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrUnsupported) || errors.Is(err, core.ErrLimit) {
			h.discardDraftReference(ctx, mailboxID, ref)
		}
		return core.Object{}, err
	}
	if markWriter != nil {
		item, err = markWriter.SetMailMarks(ctx, item.ID, item.Revision, marks)
		if err != nil {
			return core.Object{}, core.ErrOutcomeUnknown
		}
	}
	return item, nil
}

func (h *Handler) discardDraftReference(ctx context.Context, mailboxID, ref string) {
	if ref == "" {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = h.journal.store.DiscardDraftProperties(cleanup, mailboxID, ref)
}

func (h *Handler) readDraftProperties(ctx context.Context, scope core.Scope, raw []byte, shape responseShape) ([]byte, []node, error) {
	doc, err := mailmime.ReadDraft(raw)
	if err != nil {
		return raw, nil, nil
	}
	if shape.mime {
		raw = core.StripDraftReferenceHeader(raw)
	}
	if h.journal == nil || len(shape.extended) == 0 {
		return raw, nil, nil
	}
	values := doc.Header.Values(core.DraftReferenceHeader)
	if len(values) != 1 || !core.ValidDraftReference(values[0]) {
		return raw, nil, nil
	}
	binding, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return nil, nil, err
	}
	properties, err := h.journal.store.DraftProperties(ctx, binding.MailboxID, values[0])
	if err != nil || len(properties) == 0 {
		return raw, nil, err
	}
	var extensions []node
	for _, uri := range shape.extended {
		key, _ := namedStringKey(uri)
		if value, ok := properties[key]; ok {
			extensions = append(extensions, el(types, "ExtendedProperty", "", uri, el(types, "Value", string(value))))
		}
	}
	return raw, extensions, nil
}

func (h *Handler) findDraftItems(ctx context.Context, op node, session core.Session, shape responseShape) []node {
	const operation = "FindItem"
	finder, ok := session.Mailbox().(core.MailDraftReferenceFinder)
	if !ok || h.journal == nil || op.attr("Traversal") != "Shallow" || len(shape.extended) != 0 {
		return syncError(operation, core.ErrUnsupported)
	}
	page, restriction := op.child(messages, "IndexedPageItemView"), op.child(messages, "Restriction")
	sortOrder := op.child(messages, "SortOrder")
	parts := 4
	if sortOrder != nil {
		parts++
		if !isDraftCreationOrder(sortOrder) {
			return syncError(operation, core.ErrUnsupported)
		}
	}
	if page == nil || len(page.Children) != 0 || page.attr("BasePoint") != "Beginning" ||
		restriction == nil || len(restriction.Children) != 1 || len(op.Children) != parts {
		return syncError(operation, core.ErrInvalid)
	}
	limit, limErr := strconv.Atoi(page.attr("MaxEntriesReturned"))
	offset, offErr := strconv.Atoi(page.attr("Offset"))
	if limErr != nil || offErr != nil || limit < 1 || limit > 100 || offset < 0 {
		return syncError(operation, core.ErrInvalid)
	}
	match := restriction.Children[0]
	uri, constant := match.child(types, "ExtendedFieldURI"), match.child(types, "FieldURIOrConstant")
	if !isElement(match, types, "IsEqualTo") || len(match.Children) != 2 || uri == nil ||
		constant == nil || len(constant.Children) != 1 || !isElement(constant.Children[0], types, "Constant") ||
		len(constant.Children[0].Children) != 0 {
		return syncError(operation, core.ErrUnsupported)
	}
	key, err := namedStringKey(*uri)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	value := core.Text(constant.Children[0].attr("Value"))
	folder, err := h.writeTarget(ctx, session, op.child(messages, "ParentFolderIds"))
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	scope := session.Identity().Scope
	binding, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	refs, err := h.journal.store.FindDraftReferences(ctx, binding.MailboxID, key, value)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	var found []core.Object
	created := make(map[core.ID]int64)
	for _, ref := range refs {
		matches, err := finder.FindMailDraftsByReference(ctx, folder, ref.Token)
		if err != nil {
			return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
		}
		for _, item := range matches {
			if _, seen := created[item.ID]; item.Parent == folder && !seen {
				created[item.ID] = ref.CreatedAt
				found = append(found, item)
			}
		}
		if len(found) > 100 {
			return []node{writeFailure(operation, core.ErrLimit, "ErrorItemNotFound")}
		}
	}
	slices.SortStableFunc(found, func(a, b core.Object) int {
		switch {
		case created[a.ID] > created[b.ID]:
			return -1
		case created[a.ID] < created[b.ID]:
			return 1
		}
		return strings.Compare(string(b.ID), string(a.ID))
	})
	start := min(offset, len(found))
	end := min(start+limit, len(found))
	items := make([]node, 0, end-start)
	for _, item := range found[start:end] {
		items = append(items, shape.apply(h.message(scope, item)))
	}
	root := el(messages, "RootFolder", "", el(types, "Items", "", items...))
	root = at(at(at(root, "IndexedPagingOffset", strconv.Itoa(end)), "TotalItemsInView", strconv.Itoa(len(found))),
		"IncludesLastItemInRange", strconv.FormatBool(end == len(found)))
	return []node{success(operation, root)}
}

func isDraftCreationOrder(order *node) bool {
	if len(order.Children) != 1 || !isElement(order.Children[0], types, "FieldOrder") ||
		order.Children[0].attr("Order") != "Descending" || len(order.Children[0].Children) != 1 {
		return false
	}
	field := order.Children[0].Children[0]
	return isElement(field, types, "FieldURI") && field.attr("FieldURI") == "item:DateTimeCreated"
}
