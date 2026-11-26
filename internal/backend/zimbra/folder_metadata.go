// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"math"
	"strconv"
	"time"
)

// SOAP Folder.ms and Folder.md are optional. md is in seconds, not milliseconds.
// Absence stays absent; no request time, epoch or sequence-derived date fallback.
func folderFromNode(n node) (Folder, error) {
	folder := Folder{ID: n.get("id"), Parent: n.get("l"), Name: n.get("name"), View: n.get("view")}
	sequence, err := optionalFolderNumber(n, "ms")
	if err != nil {
		return Folder{}, err
	}
	date, err := optionalFolderNumber(n, "md")
	if err != nil {
		return Folder{}, err
	}
	folder.MetadataRevision = sequence
	special := contentsSpecialRole(folder.ID, true)
	if folder.View == "" || folder.View == "message" || special != 0 && folder.View == specialFolderSpec(special).view {
		folder.ContentsSequence, err = optionalFolderNumber(n, "i4ms")
		if err != nil {
			return Folder{}, err
		}
	}
	folder.ItemCount, err = optionalFolderNumber(n, "n")
	if err != nil {
		return Folder{}, err
	}
	folder.UnreadCount, err = optionalFolderNumber(n, "u")
	if err != nil {
		return Folder{}, err
	}
	if folder.ItemCount != nil && *folder.ItemCount > math.MaxInt32 || folder.UnreadCount != nil && *folder.UnreadCount > math.MaxInt32 || folder.ItemCount != nil && folder.UnreadCount != nil && *folder.UnreadCount > *folder.ItemCount {
		return Folder{}, ErrProtocol
	}
	if date != nil {
		if *date > 253402300799 {
			return Folder{}, ErrProtocol
		} // Last UTC second of year 9999.
		at := time.Unix(int64(*date), 0).UTC()
		folder.ModifiedAt = &at
	}
	return folder, nil
}

func optionalFolderNumber(n node, name string) (*uint64, error) {
	var result *uint64
	for _, a := range n.Attrs {
		if a.Name.Space != "" || a.Name.Local != name {
			continue
		}
		if result != nil {
			return nil, ErrProtocol
		}
		value, err := strconv.ParseUint(a.Value, 10, 63)
		if err != nil {
			return nil, ErrProtocol
		}
		result = &value
	}
	return result, nil
}
