// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"errors"

	"ewsgo/internal/core"
)

type contentsConfigure struct {
	frame       synchronizationConfigure
	restriction *contentsRestriction
}
type contentsSelection struct {
	onlySpecified, ignoreSpecifiedOnFAI bool
	properties                          map[contentsProperty]bool
}
type contentsFilterPlan struct {
	normal, associated, readState bool
	special                       core.SpecialFolderKind
	restriction                   *contentsRestriction
	selection                     contentsSelection
}

func (contentsConfigure) String() string    { return "contentsConfigure{private}" }
func (contentsConfigure) GoString() string  { return "contentsConfigure{private}" }
func (contentsSelection) String() string    { return "contentsSelection{private}" }
func (contentsSelection) GoString() string  { return "contentsSelection{private}" }
func (contentsFilterPlan) String() string   { return "contentsFilterPlan{private}" }
func (contentsFilterPlan) GoString() string { return "contentsFilterPlan{private}" }

// Dispatched only by the explicit Contents laboratory opt-in.
func parseContentsConfigure(r *reader, handles int) (contentsConfigure, error) {
	c, _, err := parseContentsConfigureDiagnostic(r, handles)
	return c, err
}
func parseContentsConfigureDiagnostic(r *reader, handles int) (contentsConfigure, SynchronizationOptionsDiagnostic, error) {
	var restriction *contentsRestriction
	var restrictionErr error
	frame, err := readSynchronizationConfigure(r, handles, func(raw []byte) error {
		restriction, restrictionErr = parseContentsRestriction(raw)
		return nil // Validate the complete outer frame before exposing diagnostics.
	})
	if err != nil {
		return contentsConfigure{}, SynchronizationOptionsDiagnostic{}, err
	}
	diagnostic := inspectContentsOptions(frame)
	if restrictionErr != nil {
		if !errors.Is(restrictionErr, ErrUnsupported) {
			return contentsConfigure{}, SynchronizationOptionsDiagnostic{}, restrictionErr
		}
		reason := restrictionRejected
		errors.As(restrictionErr, &reason)
		diagnostic.restrictionRejection = reason
		return contentsConfigure{}, diagnostic, restrictionErr
	}
	if frame.kind != 1 {
		return contentsConfigure{}, diagnostic, ErrUnsupported
	}
	return contentsConfigure{frame: frame, restriction: restriction}, diagnostic, nil
}

func compileContentsFilter(c contentsConfigure, registry *namedPropertyRegistry, supports func(contentsProperty) bool) (contentsFilterPlan, error) {
	plan, _, err := compileContentsFilterDiagnostic(c, registry, supports)
	return plan, err
}
func compileContentsFilterDiagnostic(c contentsConfigure, registry *namedPropertyRegistry, supports func(contentsProperty) bool) (contentsFilterPlan, contentsConfigureFailure, error) {
	if c.frame.kind != 1 {
		return contentsFilterPlan{}, contentsConfigureFailure{stage: contentsOptionsStage}, core.ErrUnsupported
	}
	diagnostic := contentsConfigureFailure{stage: contentsRestrictionStage}
	bound, err := bindContentsRestriction(c.restriction, func(tag uint32) (contentsProperty, error) {
		p, err := resolveContentsProperty(tag, registry, supports)
		if err != nil {
			diagnostic.binding = contentsBindingClass(tag)
		}
		return p, err
	})
	if err != nil {
		return contentsFilterPlan{}, diagnostic, err
	}
	selection, err := newContentsSelection(c.frame, registry)
	if err != nil {
		return contentsFilterPlan{}, contentsConfigureFailure{stage: contentsSelectionStage}, err
	}
	return contentsFilterPlan{normal: c.frame.flags&0x20 != 0, associated: c.frame.flags&0x10 != 0, readState: c.frame.flags&8 != 0, restriction: bound, selection: selection}, contentsConfigureFailure{}, nil
}

func newContentsSelection(c synchronizationConfigure, registry *namedPropertyRegistry) (contentsSelection, error) {
	if len(c.tags) > maxFolderPropertyTags {
		return contentsSelection{}, core.ErrUnsupported
	}
	out := contentsSelection{onlySpecified: c.flags&0x80 != 0, ignoreSpecifiedOnFAI: c.flags&0x4000 != 0, properties: map[contentsProperty]bool{}}
	for _, tag := range c.tags {
		kind, id := uint16(tag), uint16(tag>>16)
		// PtypUnspecified selects an ID independent of its projected type.
		// Object selectors apply only to top-level recipients/attachments.
		if !contentsScalarType(kind) && kind != 0 && kind != 0x1003 && !(kind == 0xd && (id == 0xe12 || id == 0xe13)) {
			return contentsSelection{}, core.ErrUnsupported
		}
		p, err := resolveContentsIdentity(tag, registry)
		if err != nil {
			return contentsSelection{}, err
		}
		if kind == 0x1003 && !contactSchema(p) {
			return contentsSelection{}, core.ErrUnsupported
		}
		out.properties[p] = true
	}
	return out, nil
}

// Header fields and mandatory protocol exclusions are the serializer's separate
// responsibility. This predicate selects optional top-level properties only.
func (s contentsSelection) includes(p contentsProperty, associated bool) bool {
	if s.onlySpecified && s.ignoreSpecifiedOnFAI && associated {
		return true
	}
	listed := s.properties[p]
	p.kind = 0
	listed = listed || s.properties[p]
	if s.onlySpecified {
		return listed
	}
	return !listed
}
