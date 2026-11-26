// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

const ContactIDPrefix = "contact:"

// ContactReader serves personal address book items separately from the GAL
// and from mail items. The list is complete up to the provider's read limit.
type ContactReader interface {
	ListContacts(context.Context, ID) ([]Contact, error)
	GetContact(context.Context, ID, ID) (Contact, error)
}

// TrashedContactReader exposes personal cards stored in the mailbox's native
// deleted-items folder. It never reads recoverable (dumpster) items.
type TrashedContactReader interface {
	ListTrashedContacts(context.Context, ID) ([]Contact, error)
	GetTrashedContact(context.Context, ID, ID) (Contact, error)
}

// ContactPatch merges named contact fields. An empty string clears the field.
type ContactPatch map[ContactField]string

type ContactField string

type ContactWriter interface {
	// If creation was acknowledged but readback failed, CreateContact may
	// return the assigned identity together with ErrOutcomeUnknown.
	CreateContact(context.Context, ID, ContactPatch) (Contact, error)
	UpdateContact(context.Context, Contact, ContactPatch) (Contact, error)
	TrashContact(context.Context, Contact) error
}

// ContactImportWriter correlates a create with a later native read when its
// reply is lost. The reference is internal to the backend and replica.
type ContactImportWriter interface {
	CreateContactWithReference(context.Context, ID, ContactPatch, [16]byte) (Contact, error)
	FindContactByReference(context.Context, ID, [16]byte) (Contact, error)
}

// ContactSoftDeleter removes an active contact only when the backend can
// retain it in its recoverable items area.
type ContactSoftDeleter interface {
	SoftDeleteContact(context.Context, Contact) error
}

type ContactAddress struct {
	Street, City, State, Country, PostalCode string
}

type Contact struct {
	Identity        Object   // Typed item identity, parent and revisions for protocol IDs.
	ImportReference [16]byte `json:"-"`

	FileAs, DisplayName, GivenName, MiddleName, Surname, Nickname string
	CompanyName, Department, JobTitle, WebPage, Notes             string
	Emails                                                        [3]string
	BusinessAddress, HomeAddress, OtherAddress                    ContactAddress
	BusinessPhone, BusinessPhone2, HomePhone, HomePhone2          string
	MobilePhone, BusinessFax, HomeFax, OtherPhone                 string
}
