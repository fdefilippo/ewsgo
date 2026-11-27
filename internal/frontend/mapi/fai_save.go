// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// MS-OXCMSG 2.2.3.3.1 defines three mutually exclusive save modes and
// requires servers to ignore other flags. In particular, 0x0A has the
// KeepOpenReadWrite semantics of 0x02. This does not enable delayed writes.
func faiSaveMode(flags byte) (byte, bool) {
	mode := flags & 0x07
	switch mode {
	case 0, 1, 2, 4:
		return mode, true
	case 6:
		// Outlook sends KeepOpenReadWrite and ForceSave together (0x0E
		// with an ignored bit). Gromox accepts the save but only bypasses
		// revision checks for standalone ForceSave. Preserve that check.
		return 2, true
	default:
		return 0, false
	}
}

// Save does not release a message handle. For mode zero this RAM provider
// preserves its existing access until an explicit Release or context close.
// Apply read-only access even on a clean Save, without upgrading it later.
func (d *faiDraft) finishSave(mode byte) {
	if mode == 1 {
		d.readOnly = true
	}
}
