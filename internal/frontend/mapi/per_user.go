// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

const (
	ropGetOwningServers        byte = 0x42
	ropPublicFolderIsGhosted   byte = 0x45
	ropGetPerUserLongTermIDs   byte = 0x60
	ropGetPerUserGUID          byte = 0x61
	ropReadPerUserInformation  byte = 0x63
	ropWritePerUserInformation byte = 0x64
)

// The private store has no associated public replica or saved public-folder
// read state. The ghosted reply is false for any FID on a private logon.
func privatePublicFolderState(objects *mailboxObjects, op Operation, handles []uint32) []byte {
	_, code := objects.logonObject(handles[op.Input], op.Logon)
	if code != 0 {
		return ropError(op.ID, op.Input, code)
	}
	switch op.ID {
	case ropPublicFolderIsGhosted:
		return append(ropError(op.ID, op.Input, 0), 0)
	case ropGetPerUserLongTermIDs:
		return append(ropError(op.ID, op.Input, 0), 0, 0) // LongTermIdCount.
	case ropGetPerUserGUID:
		return ropError(op.ID, op.Input, PropertyNotFound)
	case ropReadPerUserInformation:
		return append(ropError(op.ID, op.Input, 0), 1, 0, 0) // HasFinished and DataSize.
	}
	return ropError(op.ID, op.Input, NotSupported)
}
