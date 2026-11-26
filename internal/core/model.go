// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package core defines provisional protocol-independent mailbox semantics.
// Backend and protocol identifiers/cursors are deliberately different types.
package core

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound    = errors.New("object not found in this mailbox")
	ErrConflict    = errors.New("revision conflict")
	ErrUnsupported = errors.New("capability not supported")
	ErrInvalid     = errors.New("invalid semantic request")
	ErrLimit       = errors.New("request exceeds capability limit")
	ErrClosed      = errors.New("edit is closed")
	ErrResync      = errors.New("cursor requires full resynchronization")
)

// Scope is resolved from an authenticated identity by trusted application code.
// Opening a scope is not itself authentication or authorization.
type Scope struct{ Backend, Account string }
type ID string
type Revision uint64
type Cursor string
type Kind uint8

const (
	Folder Kind = iota + 1
	Message
	ContactItem
	AppointmentItem
)

// Key identifies a standard property (Set empty, ID nonzero), or a named
// property (Set GUID, a numeric ID or a string name). EmptyName distinguishes
// an empty string name from numeric ID zero. It is not an EWS/MAPI wire ID.
type Key struct {
	Set       string
	ID        uint32
	Name      string
	EmptyName bool
}

var guid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (k Key) Valid() bool {
	if k.Set == "" {
		return k.ID != 0 && !k.IsName()
	}
	return guid.MatchString(k.Set) && (!k.IsName() || k.ID == 0) && (!k.EmptyName || k.Name == "") &&
		utf8.ValidString(k.Name) && !strings.ContainsRune(k.Name, 0)
}

func (k Key) IsName() bool { return k.Name != "" || k.EmptyName }

// Values form a closed typed set. Slice values are cloned at boundaries.
type Value interface{ valueKind() string }
type Bool bool
type Integer int64
type Text string
type Binary []byte
type Timestamp time.Time
type Strings []string
type Int32s []int32

func (Bool) valueKind() string      { return "bool" }
func (Integer) valueKind() string   { return "int64" }
func (Text) valueKind() string      { return "text" }
func (Binary) valueKind() string    { return "binary" }
func (Timestamp) valueKind() string { return "time" }
func (Strings) valueKind() string   { return "strings" }
func (Int32s) valueKind() string    { return "int32s" }

type Properties map[Key]Value

func CloneProperties(p Properties) (Properties, error) {
	out := make(Properties, len(p))
	for k, v := range p {
		if !k.Valid() || v == nil {
			return nil, ErrInvalid
		}
		switch x := v.(type) {
		case Binary:
			out[k] = append(Binary(nil), x...)
		case Strings:
			out[k] = append(Strings(nil), x...)
		case Int32s:
			out[k] = append(Int32s(nil), x...)
		case Bool, Integer, Text, Timestamp:
			out[k] = x
		default:
			return nil, ErrInvalid
		}
	}
	return out, nil
}

func SameType(a, b Value) bool { return a != nil && b != nil && a.valueKind() == b.valueKind() }

// RecipientKind is semantic, independent of a frontend's wire constants.
// Unknown is useful for directory results without a message recipient role.
type RecipientKind uint8

const (
	RecipientUnknown RecipientKind = iota
	RecipientTo
	RecipientCc
	RecipientBcc
)

type Recipient struct {
	Address, DisplayName string
	Kind                 RecipientKind
}
type Attachment struct {
	ID        string
	MediaType string
	Data      []byte
}
type Object struct {
	ID          ID
	Kind        Kind
	Parent      ID
	Revision    Revision
	Properties  Properties
	MIME        []byte
	Recipients  []Recipient
	Attachments []Attachment
}

// Patch distinguishes absence from explicit replacement. Nil MIME/Recipients/
// Attachments leave data untouched; pointers to empty values clear it.
type Patch struct {
	Set         Properties
	Remove      []Key
	Parent      *ID
	MIME        *[]byte
	Recipients  *[]Recipient
	Attachments *[]Attachment
}
type Capabilities struct{ Write, Delete, Attachments, EqualQuery, Directory bool }
type Target struct {
	ID       ID
	Kind     Kind
	Parent   ID
	Expected Revision
}
type Predicate struct {
	Key   Key
	Equal Value
}
type Query struct {
	Parent        ID
	Kind          Kind
	Where         []Predicate
	Offset, Limit int
	OrderBy       *Key
}
type Page struct {
	Objects    []Object
	NextOffset int
	More       bool
}
type ChangeKind uint8

const (
	Created ChangeKind = iota + 1
	Updated
	Moved
	Deleted
)

type Change struct {
	Kind                       ChangeKind
	ID, Parent, PreviousParent ID
	Revision                   Revision
}
type Changes struct {
	Events []Change
	Next   Cursor
	More   bool
}

type Provider interface {
	Open(context.Context, Scope) (Mailbox, error)
}
type Mailbox interface {
	Capabilities() Capabilities
	Root() ID
	Get(context.Context, ID) (Object, error)
	Query(context.Context, Query) (Page, error)
	Begin(context.Context, Target) (Edit, error)
	Delete(context.Context, ID, Revision) error
	Changes(context.Context, Cursor, int) (Changes, error)
}

// Edit stages changes privately. Commit is atomic within the provider and
// checks Expected against the current revision. Remote atomicity is not implied.
type Edit interface {
	Apply(Patch) error
	Commit(context.Context) (Object, error)
	Discard()
}
type Directory interface {
	Resolve(context.Context, []string) ([]Recipient, error)
}
