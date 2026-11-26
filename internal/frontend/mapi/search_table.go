// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"slices"
	"sort"

	"ewsgo/internal/core"
)

type staticSearchSnapshot struct {
	links map[uint64][]uint64 // Original FID to sorted MIDs. Contains no message properties.
	lease *contentsLease
}

type searchTableScope struct {
	revision uint64
	sources  []searchSource
}

func (s *ramStore) isSearchFolder(fid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dynamic[s.byFID[fid].dynamic].search
}

func (scope searchTableScope) authorize(ctx context.Context, session core.Session, store *ramStore, fid uint64) error {
	criteria, code := store.readSearchCriteria(ctx, session, fid)
	switch code {
	case 0:
	case accessDenied:
		return core.ErrAccessDenied
	case PropertyNotFound:
		return core.ErrNotFound
	default:
		return core.ErrConflict
	}
	if criteria.version() != scope.revision {
		return core.ErrResync
	}
	sources, err := store.searchSources(ctx, session, fid, criteria)
	if err != nil {
		return err
	}
	if !slices.Equal(sources, scope.sources) {
		return core.ErrResync
	}
	return ctx.Err()
}

// A Search FID identifies a current result link. The opened handle uses the
// original folder and follows the ordinary message authorization path.
func resolveSearchMessageParent(ctx context.Context, session core.Session, binding storeBinding, fid, mid uint64) (uint64, error) {
	store := binding.store
	if _, err := store.messageAnchor(ctx, session, fid, false); err != nil {
		return 0, err
	}
	store.mu.Lock()
	criteria, code := store.searchCriteriaLocked(fid)
	key, known := store.byMID[mid]
	version := store.messages[key]
	parent := store.byFID[version.parent]
	store.mu.Unlock()
	if code != 0 || criteria == nil || !known || !version.present || key.associated || parent.native == "" {
		return 0, core.ErrNotFound
	}
	sources, err := store.searchSources(ctx, session, fid, criteria)
	if err != nil {
		return 0, err
	}
	if len(sources) > maxSearchSources {
		return 0, core.ErrUnsupported
	}
	if !slices.Contains(sources, searchSource{key: parent, calendar: key.special}) {
		return 0, core.ErrNotFound
	}
	predicate := criteria.predicate
	if criteria.static != nil {
		if _, found := slices.BinarySearch(criteria.static.links[version.parent], mid); !found {
			return 0, core.ErrNotFound
		}
		predicate = nil
	}
	view := contentsTable{}
	// Reserve the same retained-row budget used by the result table, but
	// capture only the original message's folder.
	view.lease, err = store.reserveContentsMemory(maxMailProjectionBytes)
	if err != nil {
		return 0, err
	}
	defer view.close()
	used := 0
	if err := view.appendSearchSource(ctx, session, binding, version.parent, key.special, predicate, mid, nil, &used); err != nil {
		return 0, err
	}
	scope := searchTableScope{revision: criteria.version(), sources: sources}
	if err := scope.authorize(ctx, session, store, fid); err != nil {
		return 0, err
	}
	if len(view.rows) == 1 {
		return version.parent, nil
	}
	return 0, core.ErrNotFound
}

func captureSearchTable(ctx context.Context, session core.Session, binding storeBinding, fid uint64, flags byte) (*contentsTable, error) {
	store := binding.store
	// The target and all sources must belong to the authenticated mailbox.
	if _, err := store.messageAnchor(ctx, session, fid, false); err != nil {
		return nil, err
	}
	store.mu.Lock()
	criteria, code := store.searchCriteriaLocked(fid)
	store.mu.Unlock()
	if code != 0 {
		return nil, core.ErrNotFound
	}
	sources, err := store.searchSources(ctx, session, fid, criteria)
	if err != nil {
		return nil, err
	}
	if len(sources) > maxSearchSources {
		return nil, core.ErrUnsupported
	}
	table, err := collectSearchRows(ctx, session, binding, criteria, sources, flags)
	if err != nil {
		return nil, err
	}
	if err := table.search.authorize(ctx, session, store, fid); err != nil {
		table.close()
		return nil, err
	}
	store.completeSearchPopulation(fid, criteria)
	return table, nil
}

func freezeSearchResults(ctx context.Context, session core.Session, binding storeBinding, criteria *folderSearchCriteria, sources []searchSource) (*staticSearchSnapshot, error) {
	table, err := collectSearchRows(ctx, session, binding, criteria, sources, 0)
	if err != nil {
		return nil, err
	}
	defer table.close()
	lease, err := binding.store.reserveContentsMemory(1024 + 64*len(table.rows))
	if err != nil {
		return nil, err
	}
	snapshot := &staticSearchSnapshot{links: make(map[uint64][]uint64), lease: lease}
	for _, row := range table.rows {
		fid := uint64(row[fixedContentsProperty(0x67480014)].(core.Integer))
		mid := uint64(row[fixedContentsProperty(0x674a0014)].(core.Integer))
		snapshot.links[fid] = append(snapshot.links[fid], mid)
	}
	for _, mids := range snapshot.links {
		slices.Sort(mids)
	}
	return snapshot, nil
}

func collectSearchRows(ctx context.Context, session core.Session, binding storeBinding, criteria *folderSearchCriteria, sources []searchSource, flags byte) (*contentsTable, error) {
	store := binding.store
	keys := make([]folderKey, len(sources))
	for i, source := range sources {
		keys[i] = source.key
	}
	fids, err := store.assign(ctx, keys)
	if err != nil {
		return nil, err
	}
	table := &contentsTable{flags: flags, search: &searchTableScope{revision: criteria.version(), sources: sources}}
	fail := func(err error) (*contentsTable, error) { table.close(); return nil, err }
	// Retained rows span every source. Admission for the next source must
	// never wait for that reservation to shrink.
	table.lease, err = store.reserveContentsMemory(maxMailProjectionBytes)
	if err != nil {
		return fail(err)
	}
	used := 1024 + 4*maxTableColumns + 64*len(sources)
	for i, source := range sources {
		used += len(source.key.native)
		predicate := criteria.predicate
		var frozen []uint64
		if criteria.static != nil {
			frozen = criteria.static.links[fids[i]]
			if len(frozen) == 0 {
				continue
			}
			predicate = nil
		}
		if err := table.appendSearchSource(ctx, session, binding, fids[i], source.calendar, predicate, 0, frozen, &used); err != nil {
			return fail(err)
		}
	}
	if err := table.lease.shrink(used); err != nil {
		return fail(err)
	}
	return table, nil
}

func (table *contentsTable) appendSearchSource(ctx context.Context, session core.Session, binding storeBinding, fid uint64, role core.SpecialFolderKind, predicate *contentsRestriction, mid uint64, frozen []uint64, used *int) error {
	store := binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
	if err != nil {
		return err
	}
	defer lease.release()
	var snapshot messageSnapshot
	if role == 0 {
		snapshot, err = store.observeMailMessages(ctx, session, fid)
	} else {
		snapshot, err = store.observeCalendarSearchItems(ctx, session, fid, role)
	}
	if err != nil {
		return err
	}
	if snapshot.replica != store.replica || snapshot.root != fid {
		return core.ErrResync
	}
	if mid != 0 {
		index := slices.IndexFunc(snapshot.messages, func(row messageRow) bool { return row.mid == mid })
		if index == -1 {
			return core.ErrNotFound
		}
		snapshot.messages = snapshot.messages[index : index+1]
	}
	if frozen != nil {
		snapshot.messages = slices.DeleteFunc(snapshot.messages, func(row messageRow) bool {
			_, found := slices.BinarySearch(frozen, row.mid)
			return !found
		})
	}
	if err := lease.shrink(snapshot.retainedBytes); err != nil {
		return err
	}
	projection, err := store.reserveContentsPeak(ctx, maxMailProjectionBytes)
	if err != nil {
		return err
	}
	defer projection.release()
	start := len(table.rows)
	if err := table.appendMessageRows(ctx, binding, snapshot, predicate, used); err != nil {
		return err
	}
	if frozen != nil {
		return table.appendMissingSearchLinks(fid, frozen, snapshot.messages, start, used)
	}
	return nil
}

// Keep the membership fixed after an original disappears. Its row no longer
// exposes any content or metadata from the original message.
func (table *contentsTable) appendMissingSearchLinks(fid uint64, frozen []uint64, live []messageRow, start int, used *int) error {
	i := 0
	for _, mid := range frozen {
		if i < len(live) && live[i].mid == mid {
			i++
			continue
		}
		*used += 1024
		if *used > maxMailProjectionBytes {
			return core.ErrUnsupported
		}
		row := contentsValues{}
		putContentsValue(row, 0x67480014, core.Integer(fid))
		putContentsValue(row, 0x674a0014, core.Integer(mid))
		putTableRowIdentity(row, mid)
		table.rows = append(table.rows, row)
	}
	rows := table.rows[start:]
	sort.Slice(rows, func(i, j int) bool {
		return uint64(table.bookmarkRow(rows[i]).id) < uint64(table.bookmarkRow(rows[j]).id)
	})
	return nil
}
