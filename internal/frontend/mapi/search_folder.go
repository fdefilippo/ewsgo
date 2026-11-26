// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync/atomic"

	"ewsgo/internal/core"
)

const (
	ropSetSearchCriteria          = 0x30
	ropGetSearchCriteria          = 0x31
	ropGetStoreState              = 0x7b
	storeHasSearches              = 0x01000000
	getSearchCriteriaResponseSize = 15
	notSearchFolder               = 0x00000461
	searchScopeViolation          = 0x00000490
	searchNotInitialized          = 0x80040605
	searchRestrictionTooComplex   = 0x80040117
	maxSearchSources              = 16
	maxSearchCriteriaDetails      = maxContentsRestrictionBytes + 8*maxSearchSources

	searchStop       = 0x00001
	searchRestart    = 0x00002
	searchRecursive  = 0x00004
	searchShallow    = 0x00008
	searchForeground = 0x00010
	searchBackground = 0x00020
	searchIndexed    = 0x10000
	searchSequential = 0x20000
	searchStatic     = 0x40000

	searchRunning       = 0x00000001
	searchRebuild       = 0x00000002
	searchStatusRecurse = 0x00000004
	searchComplete      = 0x00001000
	searchNonIndexed    = 0x08000000
)

func getStoreState(session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	object, code := objects.logonObject(handles[op.Input], op.Logon)
	if code != 0 {
		return ropError(op.ID, op.Input, code)
	}
	store := object.binding.store
	if session.Identity().Scope != store.scope {
		return ropError(op.ID, op.Input, accessDenied)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var flags uint32
	for _, folder := range store.dynamic {
		if !folder.softDeleted && folder.criteria != nil && folder.criteria.static == nil {
			flags = storeHasSearches
			break
		}
	}
	return append32(ropError(op.ID, op.Input, 0), flags)
}

type searchCriteriaOperation struct {
	unicode, includeRestriction, includeFolders bool
	limit                                       int
	flags                                       uint32
	raw                                         []byte
	folders                                     []uint64
	overLimit                                   bool
}

type folderSearchCriteria struct {
	revision   uint64
	raw        []byte
	predicate  *contentsRestriction
	folders    []uint64
	recursive  bool
	textFields []searchTextField
	populated  atomic.Bool
	static     *staticSearchSnapshot
}

func (c *folderSearchCriteria) version() uint64 {
	if c == nil {
		return 0
	}
	return c.revision
}

func (searchCriteriaOperation) String() string   { return "searchCriteriaOperation{private}" }
func (searchCriteriaOperation) GoString() string { return "searchCriteriaOperation{private}" }
func (*folderSearchCriteria) String() string     { return "folderSearchCriteria{private}" }
func (*folderSearchCriteria) GoString() string   { return "folderSearchCriteria{private}" }

func (op Operation) requestsSearchDetails() bool {
	return op.ID == ropGetSearchCriteria && (op.Search.includeRestriction || op.Search.includeFolders)
}

func parseSearchCriteriaOperation(r *reader, op *Operation) error {
	if op.ID == ropGetSearchCriteria {
		flags := r.take(3)
		if r.err != nil {
			return r.err
		}
		op.Search.unicode, op.Search.includeRestriction, op.Search.includeFolders = flags[0] != 0, flags[1] != 0, flags[2] != 0
		return nil
	}
	size := r.take(2)
	if r.err != nil {
		return r.err
	}
	raw := r.take(int(binary.LittleEndian.Uint16(size)))
	count := r.take(2)
	if r.err != nil {
		return r.err
	}
	fids := r.take(8 * int(binary.LittleEndian.Uint16(count)))
	op.Search.flags = r.u32()
	if r.err != nil {
		return r.err
	}
	if len(raw) > maxContentsRestrictionBytes || len(fids)/8 > maxSearchSources {
		op.Search.overLimit = true
		return nil
	}
	op.Search.raw = raw
	for len(fids) > 0 {
		op.Search.folders = append(op.Search.folders, binary.LittleEndian.Uint64(fids))
		fids = fids[8:]
	}
	return nil
}

func executeSearchCriteria(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32, counts *storeResults) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return result(code)
	}
	if object.isLogon || object.download != nil {
		return result(NotSupported)
	}
	store := object.binding.store
	if op.ID == ropSetSearchCriteria {
		return result(store.setSearchCriteria(ctx, session, object.binding, object.fid, op.Search, codePage, counts))
	}
	criteria, code := store.readSearchCriteria(ctx, session, object.fid)
	if code != 0 {
		return result(code)
	}

	var raw []byte
	var folders []uint64
	var status uint32
	if criteria != nil {
		if op.Search.includeRestriction {
			var ok bool
			raw, ok = searchRestrictionWire(criteria.raw, criteria.textFields, op.Search.unicode, codePage)
			if !ok {
				return result(NotSupported)
			}
		}
		if op.Search.includeFolders {
			folders = criteria.folders
		}
		status = searchRunning | searchNonIndexed
		if criteria.static != nil {
			status = searchComplete | searchNonIndexed
		} else if !criteria.populated.Load() {
			status |= searchRebuild
		}
		if criteria.recursive {
			status |= searchStatusRecurse
		}
	}
	if len(raw)+8*len(folders) > op.Search.limit {
		return result(tableBufferTooSmall)
	}
	out := binary.LittleEndian.AppendUint16(result(0), uint16(len(raw)))
	out = append(out, raw...)
	out = append(out, op.Logon)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(folders)))
	for _, fid := range folders {
		out = binary.LittleEndian.AppendUint64(out, fid)
	}
	return binary.LittleEndian.AppendUint32(out, status)
}

func (s *ramStore) readSearchCriteria(ctx context.Context, session core.Session, fid uint64) (*folderSearchCriteria, uint32) {
	folder, err := s.folder(ctx, session, fid)
	if err != nil {
		return nil, storeError(err)
	}
	if !folder.search {
		return nil, notSearchFolder
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return nil, storeError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searchCriteriaLocked(fid)
}

func (s *ramStore) searchCriteriaLocked(fid uint64) (*folderSearchCriteria, uint32) {
	folder, exists := s.dynamic[s.byFID[fid].dynamic]
	if !exists || folder.softDeleted {
		return nil, PropertyNotFound
	}
	return folder.criteria, 0
}

func (s *ramStore) setSearchCriteria(ctx context.Context, session core.Session, binding storeBinding, fid uint64, request searchCriteriaOperation, codePage uint32, counts *storeResults) uint32 {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	old, code := s.readSearchCriteria(ctx, session, fid)
	if code != 0 {
		return code
	}
	if request.overLimit {
		counts.execution.recordSearchRejection(searchRequestLimit)
		return searchRestrictionTooComplex
	}
	flags := request.flags
	if flags & ^uint32(searchStop|searchRestart|searchRecursive|searchShallow|searchForeground|searchBackground|searchIndexed|searchSequential|searchStatic) != 0 ||
		flags&(searchStop|searchRestart) == searchStop|searchRestart ||
		flags&(searchRecursive|searchShallow) == searchRecursive|searchShallow ||
		flags&(searchForeground|searchBackground) == searchForeground|searchBackground ||
		flags&(searchIndexed|searchSequential) == searchIndexed|searchSequential {
		return 0x80070057
	}
	if old == nil && (len(request.folders) == 0 || len(request.raw) == 0) {
		return searchNotInitialized
	}
	if flags&searchIndexed != 0 {
		return NotSupported
	}
	if flags&searchRestart == 0 {
		// Completed dynamic searches keep running when stopped. There is
		// no background worker to cancel, and we cannot stage STOP updates.
		if old != nil && len(request.folders) == 0 && len(request.raw) == 0 {
			return 0
		}
		return NotSupported
	}

	next := &folderSearchCriteria{revision: old.version() + 1, recursive: flags&searchRecursive != 0}
	if old != nil {
		next.raw, next.predicate, next.textFields, next.folders = old.raw, old.predicate, old.textFields, old.folders
	}
	if len(request.raw) != 0 {
		text := searchRestrictionText{codePage: codePage}
		restriction, err := parseContentsRestrictionText(request.raw, &text)
		if errors.Is(err, ErrMalformed) {
			return 0x80070057
		}
		if err != nil {
			counts.execution.recordSearchRejection(searchRestrictionShape)
			if counts.execution.enabled && counts.execution.searchRestriction == 0 {
				errors.As(err, &counts.execution.searchRestriction)
				errors.As(err, &counts.execution.searchRestrictionType)
			}
			return searchRestrictionTooComplex
		}
		normalized, ok := searchRestrictionWire(request.raw, text.fields, true, codePage)
		if !ok || len(normalized) > maxContentsRestrictionBytes {
			counts.execution.recordSearchRejection(searchRequestLimit)
			return searchRestrictionTooComplex
		}
		next.predicate, err = s.bindSearchPredicate(restriction, counts)
		if err != nil {
			counts.execution.recordSearchRejection(searchRestrictionBinding)
			return searchRestrictionTooComplex
		}
		next.raw = bytes.Clone(request.raw)
		next.textFields = text.fields
	}
	if len(request.folders) != 0 {
		next.folders = slices.Clone(request.folders)
	}
	sources, err := s.searchSources(ctx, session, fid, next)
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, errSearchScope) {
		return searchScopeViolation
	}
	if err != nil {
		return storeError(err)
	}
	if len(sources) > maxSearchSources {
		counts.execution.recordSearchRejection(searchExpandedScopeLimit)
		return searchRestrictionTooComplex
	}

	if flags&searchStatic != 0 {
		// Table admission can wait for temporary workspaces, including a
		// hierarchy capture. Revalidate the search after releasing this lock.
		s.hierarchyMu.Unlock()
		snapshot, err := freezeSearchResults(ctx, session, binding, next, sources)
		s.hierarchyMu.Lock()
		if err != nil {
			return storeError(err)
		}
		again, err := s.searchSources(ctx, session, fid, next)
		if err == nil && !slices.Equal(again, sources) {
			err = core.ErrResync
		}
		if err != nil {
			snapshot.lease.release()
			return storeError(err)
		}
		next.static = snapshot
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		next.releaseStatic()
		return storeError(err)
	}
	current, code := s.searchCriteriaLocked(fid)
	if code != 0 {
		next.releaseStatic()
		return code
	}
	if current != old {
		next.releaseStatic()
		return storeError(core.ErrConflict)
	}
	if err := s.persistSearchLocked(ctx, fid, next, codePage); err != nil {
		next.releaseStatic()
		return storeError(err)
	}
	key := s.byFID[fid]
	folder := s.dynamic[key.dynamic]
	folder.criteria = next
	s.dynamic[key.dynamic] = folder
	if next.static != nil {
		s.publishSearchCompletionLocked(fid, next)
	}
	old.releaseStatic()
	return 0
}

func (s *ramStore) bindSearchPredicate(restriction *contentsRestriction, counts *storeResults) (*contentsRestriction, error) {
	normalSchema := contentsProjectionSchema(true, false)
	return bindContentsRestriction(restriction, func(tag uint32) (contentsProperty, error) {
		property, rejection := resolveSearchProperty(tag, &s.named, func(p contentsProperty) bool {
			return normalSchema(p) || isCalendarSearchProperty(p)
		})
		if rejection != 0 {
			counts.execution.recordSearchBinding(rejection, uint16(tag))
			return contentsProperty{}, core.ErrUnsupported
		}
		return property, nil
	})
}

// The dynamic view populates on its first successful read. An older
// concurrent capture must not announce completion for a replaced definition.
func (s *ramStore) completeSearchPopulation(fid uint64, criteria *folderSearchCriteria) {
	if criteria == nil || criteria.populated.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, code := s.searchCriteriaLocked(fid)
	if code == 0 && current == criteria {
		s.publishSearchCompletionLocked(fid, criteria)
	}
}

func (s *ramStore) publishSearchCompletionLocked(fid uint64, criteria *folderSearchCriteria) {
	if criteria.populated.CompareAndSwap(false, true) {
		s.localNotifications.publish(objectNotification{flag: notifySearchCompleted, fid: fid})
	}
}

func (c *folderSearchCriteria) releaseStatic() {
	if c != nil && c.static != nil {
		c.static.lease.release()
	}
}

func resolveSearchProperty(tag uint32, registry *namedPropertyRegistry, supports func(contentsProperty) bool) (contentsProperty, searchBindingRejection) {
	if tag == parentServerIDTag {
		return parentServerIDProperty, 0
	}
	if !contentsScalarType(uint16(tag)) {
		return contentsProperty{}, searchBindingType
	}
	property, err := resolveContentsIdentity(tag, registry)
	if err != nil {
		if id := uint16(tag >> 16); id == 0 || id == 0x8000 || id == 0xffff {
			return contentsProperty{}, searchBindingReserved
		}
		return contentsProperty{}, searchBindingUnregistered
	}
	if !supports(property) {
		if property.id == 0 {
			return contentsProperty{}, searchBindingNamed
		}
		return contentsProperty{}, searchBindingFixed
	}
	return property, 0
}

var errSearchScope = errors.New("invalid Search folder scope")

type searchSource struct {
	key      folderKey
	calendar core.SpecialFolderKind
}

// A fresh private hierarchy proves scope visibility and membership for each
// capture. Search folders cannot serve as sources or appear in their own scope.
func (s *ramStore) searchSources(ctx context.Context, session core.Session, fid uint64, criteria *folderSearchCriteria) ([]searchSource, error) {
	if criteria == nil {
		return nil, nil
	}
	nodes, err := s.currentProjection(ctx, session)
	if err != nil {
		return nil, err
	}
	roots := make(map[folderKey]bool, len(criteria.folders))
	s.mu.Lock()
	target := s.byFID[fid]
	for _, source := range criteria.folders {
		key, ok := s.byFID[source]
		if !ok || key == target {
			s.mu.Unlock()
			return nil, errSearchScope
		}
		roots[key] = true
	}
	s.mu.Unlock()

	var sources []searchSource
	rootsFound := 0
	selected := make(map[folderKey]bool)
	matchesWithoutCalendar := searchMayMatchWithoutCalendarProperties(criteria.predicate)
	calendarSchema := searchUsesOnlyCalendarSchema(criteria.predicate)
	_, calendarReader := session.Mailbox().(core.CalendarSearchReader)
	for _, node := range nodes {
		root := roots[node.key]
		if root {
			rootsFound++
		}
		if !root && !(criteria.recursive && selected[node.parent]) {
			continue
		}
		if node.key == target || root && node.search {
			return nil, errSearchScope
		}
		if node.search {
			continue
		}
		selected[node.key] = true
		if node.key.native == "" {
			continue
		}
		role := searchCalendarRole(node.containerClass)
		switch {
		case role != 0:
			if !calendarReader || !calendarSchema {
				return nil, core.ErrUnsupported
			}
		case !matchesWithoutCalendar && (node.containerClass == "IPF.Contact" || node.containerClass == "" || node.containerClass == "IPF.Note"):
			// These projections contain none of the calendar-only properties.
			// Keep traversing their children, even when their own rows cannot match.
			continue
		case node.containerClass != "" && node.containerClass != "IPF.Note":
			return nil, core.ErrUnsupported
		}
		sources = append(sources, searchSource{node.key, role})
	}
	if rootsFound != len(roots) {
		return nil, core.ErrNotFound
	}
	return sources, nil
}
