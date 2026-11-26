// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const (
	maxMailProjectionBytes = 4 << 20
	maxMailParts           = 128
	maxMailPartDepth       = 16
	maxMailAttachments     = 64
	maxMailHeaderBytes     = 64 << 10
	flagStatusTag          = 0x10900003
	flagCompleteTimeTag    = 0x10910040
	followupIconTag        = 0x10950003
)

// Private projection, never an authorization or ownership boundary. Its caller
// must reserve the aggregate contents budget before capturing/decoding data.
type projectedMessage struct {
	values                  contentsValues
	recipients, attachments []contentsValues
	embedded                map[int]projectedMessage
	embeddedRecipients      []core.Recipient
	size, bytes             int
}

func (projectedMessage) String() string   { return "projectedMessage{private}" }
func (projectedMessage) GoString() string { return "projectedMessage{private}" }

func fixedContentsProperty(tag uint32) contentsProperty {
	return contentsProperty{id: uint16(tag >> 16), kind: uint16(tag)}
}
func putContentsValue(values contentsValues, tag uint32, value core.Value) {
	values[fixedContentsProperty(tag)] = value
}

func projectMailContents(o core.Object) (projectedMessage, error) {
	return projectMailContentsAtDepth(o, 0, &mailProjectionBudget{})
}

func projectMailContentsAtDepth(o core.Object, depth int, budget *mailProjectionBudget) (projectedMessage, error) {
	if o.Kind != core.Message || len(o.MIME) == 0 || len(o.MIME) > core.MaxMailContentMIMEBytes || len(o.Attachments) != 0 || len(o.Recipients) > 256 || depth > maxMailPartDepth {
		return projectedMessage{}, core.ErrUnsupported
	}
	o.MIME = core.StripDraftReferenceHeader(o.MIME)
	message, err := mail.ReadMessage(bytes.NewReader(o.MIME))
	if err != nil {
		return projectedMessage{}, core.ErrInvalid
	}
	headerEnd := bytes.Index(o.MIME, []byte("\r\n\r\n"))
	separator := 4
	if lf := bytes.Index(o.MIME, []byte("\n\n")); lf >= 0 && (headerEnd < 0 || lf < headerEnd) {
		headerEnd, separator = lf, 2
	}
	if headerEnd < 0 || headerEnd > maxMailHeaderBytes {
		return projectedMessage{}, core.ErrUnsupported
	}
	headers := string(o.MIME[:headerEnd+separator])
	if !validMailText(headers) {
		return projectedMessage{}, core.ErrUnsupported
	}
	h := textproto.MIMEHeader(message.Header)
	if err := validateMailHeaders(h); err != nil {
		return projectedMessage{}, err
	}
	if depth != 0 {
		o.Recipients, err = embeddedMailRecipients(h)
		if err != nil {
			return projectedMessage{}, err
		}
	}
	result := projectedMessage{values: contentsValues{}, size: len(o.MIME), bytes: 1024 + len(headers)*2}
	if depth != 0 {
		result.embeddedRecipients = o.Recipients
	}
	if err := projectMailContentsMetadata(result.values, o); err != nil {
		return projectedMessage{}, err
	}
	putContentsValue(result.values, 0x007d001f, core.Text(headers))
	decoder := mime.WordDecoder{CharsetReader: mailCharsetReader}
	for key, tag := range map[string]uint32{"Subject": 0x0037001f, "Message-ID": 0x1035001f} {
		if raw := h.Get(key); raw != "" {
			value, err := decoder.DecodeHeader(raw)
			if err != nil || !validMailText(value) {
				return projectedMessage{}, core.ErrUnsupported
			}
			putContentsValue(result.values, tag, core.Text(value))
			result.bytes += 2*len(value) + 128
		}
	}
	if err := projectMailConversation(&result, h); err != nil {
		return projectedMessage{}, err
	}
	if raw := h.Get("Date"); raw != "" {
		date, err := mail.ParseDate(raw)
		if err != nil || date.Year() < 1601 || date.Year() > 9999 {
			return projectedMessage{}, core.ErrUnsupported
		}
		putContentsValue(result.values, 0x00390040, core.Timestamp(date.UTC()))
	}
	parser := mailProjectionParser{result: &result, budget: budget}
	if err := parser.part(h, message.Body, depth, true); err != nil {
		return projectedMessage{}, err
	}
	if parser.plain != nil {
		putContentsValue(result.values, 0x1000001f, core.Text(*parser.plain))
	}
	if parser.html != nil {
		putContentsValue(result.values, 0x10130102, core.Binary(parser.html))
		putContentsValue(result.values, 0x3fde0003, core.Integer(parser.htmlCodePage))
		putContentsValue(result.values, 0x10160003, core.Integer(3))
		for _, attachment := range result.attachments {
			if attachment[fixedContentsProperty(0x3712001f)] != nil || attachment[fixedContentsProperty(0x3713001f)] != nil {
				putContentsValue(attachment, 0x37140003, core.Integer(4)) // ATT_MHTML_REF.
			}
		}
	} else if parser.plain != nil {
		putContentsValue(result.values, 0x10160003, core.Integer(1))
	}
	if parser.rtf != nil {
		putContentsValue(result.values, messageRTFTag, core.Binary(parser.rtf))
		putContentsValue(result.values, messageNativeBody, core.Integer(2))
	}
	if err := projectMailSenders(&result, message.Header); err != nil {
		return projectedMessage{}, err
	}
	for i, recipient := range o.Recipients {
		if recipient.Kind < core.RecipientTo || recipient.Kind > core.RecipientBcc || !validMailText(recipient.DisplayName) {
			return projectedMessage{}, core.ErrUnsupported
		}
		address, err := mail.ParseAddress(recipient.Address)
		if err != nil || address.Name != "" || address.Address != recipient.Address {
			return projectedMessage{}, core.ErrInvalid
		}
		row := contentsValues{}
		putContentsValue(row, 0x30000003, core.Integer(i)) // PidTagRowid, local to this full recipient list.
		role := map[core.RecipientKind]int64{core.RecipientTo: 1, core.RecipientCc: 2, core.RecipientBcc: 3}[recipient.Kind]
		putContentsValue(row, 0x0c150003, core.Integer(role))
		putContentsValue(row, 0x3002001f, core.Text("SMTP"))
		putContentsValue(row, 0x3003001f, core.Text(recipient.Address))
		putContentsValue(row, 0x39fe001f, core.Text(recipient.Address))
		putContentsValue(row, 0x3001001f, core.Text(recipient.DisplayName))
		result.recipients = append(result.recipients, row)
		result.bytes += 1024 + 4*len(recipient.Address) + 2*len(recipient.DisplayName)
	}
	hasAttachments := len(result.attachments) != 0
	putContentsValue(result.values, 0x0e1b000b, core.Bool(hasAttachments))
	putContentsValue(result.values, 0x0e070003, core.Integer(mailContentsFlags(o, hasAttachments)))
	if result.bytes > maxMailProjectionBytes {
		return projectedMessage{}, core.ErrUnsupported
	}
	return result, nil
}

func projectMailContentsMetadata(values contentsValues, o core.Object) error {
	putContentsValue(values, 0x001a001f, core.Text("IPM.Note"))
	for key, tag := range map[core.Key]uint32{core.ReceivedAt: 0x0e060040, core.IsRead: 0x0e69000b, core.DeferredActionsPending: hasDAMs} {
		if value := o.Properties[key]; value != nil {
			if !contentsValueType(uint16(tag), value) {
				return core.ErrInvalid
			}
			putContentsValue(values, tag, value)
		}
	}
	projectMailFollowUp(values, o.Properties)
	return nil
}

func projectMailFollowUp(values contentsValues, properties core.Properties) {
	flag := core.MailFollowUpOf(properties)
	switch flag.Status {
	case core.FollowUpMarked:
		putContentsValue(values, flagStatusTag, core.Integer(2))
		putContentsValue(values, followupIconTag, core.Integer(6)) // Red follow-up flag.
	case core.FollowUpComplete:
		putContentsValue(values, flagStatusTag, core.Integer(1))
		if completed := flag.Complete.UTC(); completed.Year() >= 1601 && completed.Year() <= 9999 {
			putContentsValue(values, flagCompleteTimeTag, core.Timestamp(completed.Truncate(time.Minute)))
		}
	}
}

func mailContentsFlags(o core.Object, hasAttachments bool) int64 {
	flags := int64(0)
	if o.Properties[core.IsRead] == core.Bool(true) {
		flags |= 1
	}
	if hasAttachments {
		flags |= 0x10
	}
	return flags
}

func embeddedMailRecipients(header textproto.MIMEHeader) ([]core.Recipient, error) {
	parser := mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: mailCharsetReader}}
	var recipients []core.Recipient
	for _, item := range []struct {
		header string
		kind   core.RecipientKind
	}{{"To", core.RecipientTo}, {"Cc", core.RecipientCc}, {"Bcc", core.RecipientBcc}} {
		for _, field := range header.Values(item.header) {
			if strings.TrimSpace(field) == "" {
				continue
			}
			addresses, err := parser.ParseList(field)
			if err != nil {
				return nil, core.ErrInvalid
			}
			if len(recipients)+len(addresses) > 256 {
				return nil, core.ErrUnsupported
			}
			for _, address := range addresses {
				recipients = append(recipients, core.Recipient{Kind: item.kind, Address: address.Address, DisplayName: address.Name})
			}
		}
	}
	return recipients, nil
}

func projectMailSenders(result *projectedMessage, header mail.Header) error {
	parser := mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: mailCharsetReader}}
	var from *mail.Address
	for _, item := range []struct {
		name string
		tags [4]uint32
	}{
		{"From", [4]uint32{0x0042001f, 0x0064001f, 0x0065001f, 0x5d02001f}},
		{"Sender", [4]uint32{0x0c1a001f, 0x0c1e001f, 0x0c1f001f, 0x5d01001f}},
	} {
		raw := header.Get(item.name)
		var address *mail.Address
		if raw != "" {
			addresses, err := parser.ParseList(raw)
			if err != nil || len(addresses) != 1 {
				return core.ErrUnsupported
			}
			address = addresses[0]
		} else if item.name == "Sender" {
			address = from
		}
		if address == nil {
			continue
		}
		if !validMailText(address.Name) || !validMailText(address.Address) {
			return core.ErrInvalid
		}
		if item.name == "From" {
			from = address
		}
		for i, value := range []string{address.Name, "SMTP", address.Address, address.Address} {
			putContentsValue(result.values, item.tags[i], core.Text(value))
			result.bytes += 2*len(value) + 128
		}
	}
	return nil
}

type mailProjectionParser struct {
	result       *projectedMessage
	budget       *mailProjectionBudget
	plain        *string
	html         []byte
	rtf          []byte
	htmlCodePage int
}

type mailProjectionBudget struct{ parts, decoded int }

func (*mailProjectionParser) String() string   { return "mailProjectionParser{private}" }
func (*mailProjectionParser) GoString() string { return "mailProjectionParser{private}" }

func (p *mailProjectionParser) part(h textproto.MIMEHeader, input io.Reader, depth int, body bool) error {
	p.budget.parts++
	if depth > maxMailPartDepth || p.budget.parts > maxMailParts {
		return core.ErrUnsupported
	}
	if err := validateMailHeaders(h); err != nil {
		return err
	}
	media, params := "text/plain", map[string]string{}
	if raw := h.Get("Content-Type"); raw != "" {
		var err error
		media, params, err = mime.ParseMediaType(raw)
		if err != nil {
			return core.ErrInvalid
		}
	}
	disposition, dp := "", map[string]string{}
	if raw := h.Get("Content-Disposition"); raw != "" {
		var err error
		disposition, dp, err = mime.ParseMediaType(raw)
		if err != nil {
			return core.ErrInvalid
		}
		if disposition != "inline" && disposition != "attachment" {
			return core.ErrUnsupported
		}
	}
	filename := dp["filename"]
	if filename == "" {
		filename = params["name"]
	}
	var err error
	filename, err = (&mime.WordDecoder{CharsetReader: mailCharsetReader}).DecodeHeader(filename)
	if err != nil || !validMailText(filename) {
		return core.ErrUnsupported
	}
	encoding := strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding")))
	if strings.HasPrefix(media, "multipart/") {
		if encoding != "" && encoding != "7bit" && encoding != "8bit" && encoding != "binary" {
			return core.ErrUnsupported
		}
		if disposition == "attachment" || filename != "" || !body {
			return core.ErrUnsupported
		}
		if media != "multipart/mixed" && media != "multipart/alternative" && media != "multipart/related" {
			return core.ErrUnsupported
		}
		return p.multipart(media, params, input, depth)
	}
	if media != "message/rfc822" && strings.HasPrefix(media, "message/") || media == "application/ms-tnef" || strings.Contains(media, "pkcs7") {
		return core.ErrUnsupported
	}
	data, err := p.decodePart(input, encoding)
	if err != nil {
		return err
	}
	isBody := body && disposition != "attachment" && filename == ""
	if isBody && media == "text/rtf" {
		if p.rtf != nil {
			return core.ErrUnsupported
		}
		rtf, err := readImportRTF(data)
		if err != nil {
			return err
		}
		p.rtf = uncompressedRTFEnvelope(data)
		p.result.bytes += len(p.rtf)
		if p.plain == nil {
			p.plain = &rtf.plain
			p.result.bytes += 2*len(rtf.plain) + 128
		}
		return nil
	}
	if isBody && (media == "text/plain" || media == "text/html") {
		text, err := decodeMailText(params["charset"], data)
		if err != nil || !validMailText(text) {
			return core.ErrUnsupported
		}
		p.result.bytes += 2*len(text) + 128
		if p.result.bytes > maxMailProjectionBytes {
			return core.ErrUnsupported
		}
		if media == "text/plain" {
			p.plain = &text
		} else {
			// Preserve original HTML bytes and their code page. Transcoding only
			// the bytes would contradict an embedded meta charset declaration.
			p.html = append([]byte{}, data...)
			p.htmlCodePage, _ = mailCharsetCodePage(params["charset"])
		}
		return nil
	}
	if len(p.result.attachments) >= maxMailAttachments {
		return core.ErrUnsupported
	}
	if media == "text/calendar" && filename == "" {
		// Preserve iCalendar content in the mail projection. Creating a MAPI
		// MeetingRequest requires a separate projection of the native invite.
		filename = "calendar.ics"
	}
	row := contentsValues{}
	number := len(p.result.attachments)
	putContentsValue(row, 0x0e210003, core.Integer(number))
	if media == "message/rfc822" {
		embedded, err := projectMailContentsAtDepth(core.Object{Kind: core.Message, MIME: data}, depth+1, p.budget)
		if err != nil {
			return err
		}
		putContentsValue(row, 0x37050003, core.Integer(5)) // ATTACH_EMBEDDED_MSG.
		if subject := embedded.values[fixedContentsProperty(0x0037001f)]; subject != nil {
			putContentsValue(row, 0x3001001f, subject)
		}
		if p.result.embedded == nil {
			p.result.embedded = make(map[int]projectedMessage)
		}
		p.result.embedded[number] = embedded
		p.result.bytes += embedded.bytes
	} else {
		putContentsValue(row, 0x37050003, core.Integer(1)) // ATTACH_BY_VALUE.
		putContentsValue(row, 0x37010102, core.Binary(data))
		p.result.bytes += len(data)
	}
	putContentsValue(row, 0x0e200003, core.Integer(len(data)))
	putContentsValue(row, 0x370e001f, core.Text(media))
	putContentsValue(row, 0x370b0003, core.Integer(-1))
	if filename != "" {
		putContentsValue(row, 0x3707001f, core.Text(filename))
		putContentsValue(row, 0x3704001f, core.Text(filename))
		if dot := strings.LastIndexByte(filename, '.'); dot > 0 && dot < len(filename)-1 {
			extension := filename[dot:]
			if len(extension) < 16 && !strings.ContainsAny(extension, `/\`) {
				putContentsValue(row, 0x3703001f, core.Text(extension))
				p.result.bytes += 2 * len(extension)
			}
		}
	}
	for key, tag := range map[string]uint32{"Content-ID": 0x3712001f, "Content-Location": 0x3713001f} {
		value := h.Get(key)
		if key == "Content-ID" && value != "" {
			if !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") {
				return core.ErrInvalid
			}
			value = value[1 : len(value)-1]
			if value == "" {
				return core.ErrInvalid
			}
		}
		if !validMailText(value) {
			return core.ErrUnsupported
		}
		if value != "" {
			putContentsValue(row, tag, core.Text(value))
			p.result.bytes += 2*len(value) + 128
		}
	}
	p.result.bytes += 2*len(filename) + 2048
	if p.result.bytes > maxMailProjectionBytes {
		return core.ErrUnsupported
	}
	p.result.attachments = append(p.result.attachments, row)
	return nil
}

func (p *mailProjectionParser) multipart(media string, params map[string]string, input io.Reader, depth int) error {
	boundary := params["boundary"]
	if boundary == "" || len(boundary) > 70 {
		return core.ErrInvalid
	}
	r := multipart.NewReader(input, boundary)
	count, rootCount := 0, 0
	start := params["start"]
	for {
		part, err := r.NextRawPart() // NextPart would silently decode quoted-printable.
		if err == io.EOF {
			break
		}
		if err != nil {
			return core.ErrInvalid
		}
		root := count == 0
		if media == "multipart/related" && start != "" {
			root = part.Header.Get("Content-ID") == start
		}
		allowBody := media == "multipart/alternative" || root
		if media == "multipart/mixed" {
			allowBody = p.plain == nil && p.html == nil
		}
		if media == "multipart/related" && root {
			rootCount++
			kind, _, e := mime.ParseMediaType(part.Header.Get("Content-Type"))
			declared, present := params["type"]
			// RFC 2387 requires type. For received mail omitting it, accept
			// only an explicit text/alternative root: no sniffing or rewriting.
			// A supplied contradictory/empty type remains unsupported.
			if e != nil || present && !strings.EqualFold(declared, kind) || !present && kind != "text/plain" && kind != "text/html" && kind != "multipart/alternative" {
				return core.ErrUnsupported
			}
		}
		before := len(p.result.attachments)
		if err := p.part(part.Header, part, depth+1, allowBody); err != nil {
			return err
		}
		if media == "multipart/alternative" && len(p.result.attachments) != before {
			childType, _, e := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if e != nil || childType != "multipart/related" && childType != "text/calendar" {
				return core.ErrUnsupported
			}
		}
		count++
	}
	if count == 0 || media == "multipart/related" && rootCount != 1 {
		return core.ErrInvalid
	}
	return nil
}

func (p *mailProjectionParser) decodePart(input io.Reader, encoding string) ([]byte, error) {
	if encoding == "quoted-printable" {
		raw, err := io.ReadAll(io.LimitReader(input, core.MaxMailContentMIMEBytes+1))
		if err != nil || len(raw) > core.MaxMailContentMIMEBytes {
			return nil, core.ErrUnsupported
		}
		for i := 0; i < len(raw); i++ {
			if raw[i] != '=' {
				continue
			}
			if i+1 < len(raw) && raw[i+1] == '\n' {
				i++
				continue
			}
			if i+2 < len(raw) && raw[i+1] == '\r' && raw[i+2] == '\n' {
				i += 2
				continue
			}
			if i+2 >= len(raw) || !mailHex(raw[i+1]) || !mailHex(raw[i+2]) {
				return nil, core.ErrInvalid
			}
			i += 2
		}
		input = bytes.NewReader(raw)
	}
	switch encoding {
	case "", "7bit", "8bit", "binary":
	case "base64":
		input = base64.NewDecoder(base64.StdEncoding, input)
	case "quoted-printable":
		input = quotedprintable.NewReader(input)
	default:
		return nil, core.ErrUnsupported
	}
	data, err := io.ReadAll(io.LimitReader(input, int64(core.MaxMailContentMIMEBytes)+1))
	if err != nil {
		return nil, core.ErrInvalid
	}
	p.budget.decoded += len(data)
	if len(data) > core.MaxMailContentMIMEBytes || p.budget.decoded > maxMailProjectionBytes {
		return nil, core.ErrUnsupported
	}
	if encoding == "7bit" {
		for _, b := range data {
			if b >= 128 {
				return nil, core.ErrInvalid
			}
		}
	}
	return data, nil
}

func mailHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func validateMailHeaders(h textproto.MIMEHeader) error {
	used, fields := 0, 0
	for key, values := range h {
		fields += len(values)
		for _, value := range values {
			used += len(key) + len(value) + 4
		}
	}
	if used > maxMailHeaderBytes || fields > 256 {
		return core.ErrUnsupported
	}
	for _, key := range []string{"Content-Type", "Content-Disposition", "Content-Transfer-Encoding", "Content-ID", "Content-Location", "Subject", "Thread-Topic", "Thread-Index", "Message-ID", "Date", "From", "Sender"} {
		if len(h.Values(key)) > 1 {
			return core.ErrUnsupported
		}
	}
	return nil
}
func validMailText(text string) bool { return utf8.ValidString(text) && !strings.ContainsRune(text, 0) }
func mailCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(io.LimitReader(input, core.MaxMailContentMIMEBytes+1))
	if err != nil || len(data) > core.MaxMailContentMIMEBytes {
		return nil, core.ErrUnsupported
	}
	text, err := decodeMailText(charset, data)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(text), nil
}
func decodeMailText(charset string, data []byte) (string, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "utf8":
		if !utf8.Valid(data) {
			return "", core.ErrInvalid
		}
		return string(data), nil
	case "", "us-ascii", "ascii":
		for _, b := range data {
			if b >= 128 {
				return "", core.ErrUnsupported
			}
		}
		return string(data), nil
	case "iso-8859-1", "latin1", "windows-1252", "cp1252":
		windows := strings.EqualFold(charset, "windows-1252") || strings.EqualFold(charset, "cp1252")
		var text strings.Builder
		for _, b := range data {
			c := rune(b)
			if windows && b >= 0x80 && b <= 0x9f {
				c = cp1252[b-0x80]
			}
			text.WriteRune(c)
		}
		return text.String(), nil
	default:
		return "", core.ErrUnsupported
	}
}

func mailCharsetCodePage(charset string) (int, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "utf8":
		return 65001, nil
	case "", "us-ascii", "ascii":
		return 20127, nil
	case "iso-8859-1", "latin1":
		return 28591, nil
	case "windows-1252", "cp1252":
		return 1252, nil
	default:
		return 0, core.ErrUnsupported
	}
}
