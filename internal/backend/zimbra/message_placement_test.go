// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

func TestMailMessagePlacementChecksScopeAndOutcome(t *testing.T) {
	tests := []struct {
		name, operation, outcome string
		source, parent, target   core.ID
		revision                 core.Revision
		write                    bool
		wantID                   core.ID
		wantErr                  error
	}{
		{name: "move", operation: "move", write: true, wantID: "m:9"},
		{name: "copy", operation: "copy", write: true, wantID: "m:10"},
		{name: "already in target", operation: "move", parent: "f:5", wantID: "m:9"},
		{name: "trash", operation: "trash", write: true, wantID: "m:9"},
		{name: "already in Trash", operation: "trash", parent: "f:3", wantID: "m:9"},
		{name: "restore", operation: "restore", parent: "f:3", write: true, wantID: "m:9"},
		{name: "restore requires Trash", operation: "restore", wantErr: core.ErrConflict},
		{name: "stale source revision", operation: "copy", revision: 6, wantErr: core.ErrConflict},
		{name: "revision outside native range", operation: "move", revision: 1 << 31, wantErr: core.ErrInvalid},
		{name: "missing source", operation: "move", outcome: "missing source", wantErr: core.ErrNotFound},
		{name: "shared destination", operation: "copy", target: "f:21", wantErr: core.ErrNotFound},
		{name: "qualified destination", operation: "copy", target: "f:foreign:5", wantErr: core.ErrInvalid},
		{name: "qualified source", operation: "move", source: "m:foreign:9", wantErr: core.ErrInvalid},
		{name: "shared origin", operation: "move", parent: "f:21", wantErr: core.ErrNotFound},
		{name: "lost acknowledgment", operation: "copy", outcome: "transport loss", write: true, wantErr: core.ErrOutcomeUnknown},
		{name: "missing new ID", operation: "copy", outcome: "missing ID", write: true, wantErr: core.ErrOutcomeUnknown},
		{name: "unclear write fault", operation: "copy", outcome: "service.FAILURE", write: true, wantErr: core.ErrOutcomeUnknown},
		{name: "native move conflict", operation: "move", outcome: "mail.MODIFY_CONFLICT", write: true, wantErr: core.ErrConflict},
		{name: "native copy conflict", operation: "copy", outcome: "mail.MODIFY_CONFLICT", write: true, wantErr: core.ErrConflict},
		{name: "write denied", operation: "move", outcome: "service.PERM_DENIED", write: true, wantErr: core.ErrAccessDenied},
		{name: "readback missing", operation: "copy", outcome: "readback missing", write: true, wantID: "m:10", wantErr: core.ErrOutcomeUnknown},
		{name: "concurrent move after write", operation: "move", outcome: "wrong parent", write: true, wantID: "m:9", wantErr: core.ErrOutcomeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.source == "" {
				tt.source = "m:9"
			}
			if tt.parent == "" {
				tt.parent = "f:2"
			}
			if tt.target == "" {
				tt.target = "f:5"
			}
			if tt.operation == "trash" {
				tt.target = "f:3"
			}
			if tt.revision == 0 {
				tt.revision = 7
			}
			var actions atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				req := readMessageRequest(t, r)
				switch req.XMLName.Local {
				case "GetFolderRequest":
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="root"><folder id="2" l="1" name="Inbox"/><folder id="3" l="1" name="Trash"/><folder id="5" l="1" name="Sent"/><link id="21" l="1" name="Shared"/></folder></GetFolderResponse>`)
				case "GetMsgRequest":
					if tt.outcome == "missing source" || actions.Load() > 0 && tt.outcome == "readback missing" {
						fault(w, "mail.NO_SUCH_MSG")
						return
					}
					id, parent, revision := "9", tt.parent[2:], 7
					if actions.Load() > 0 {
						parent, revision = tt.target[2:], 8
						if tt.operation == "copy" {
							id = "10"
						}
						if tt.outcome == "wrong parent" {
							parent = "2"
						}
					}
					selector := req.child(mailNS, "m")
					if selector == nil || selector.get("id") != id || selector.get("read") != "0" {
						t.Error("unexpected read selector")
					}
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="`+id+`" l="`+string(parent)+`" ms="`+strconv.Itoa(revision)+`" rev="3"/></GetMsgResponse>`)
				case "MsgActionRequest", "ItemActionRequest":
					actions.Add(1)
					wireOperation, wireAction, nci, created := "MsgAction", "move", "", ""
					if tt.operation == "copy" {
						wireOperation, wireAction, nci, created = "ItemAction", "copy", "1", ` nci="10"`
					}
					action := req.child(mailNS, "action")
					if req.XMLName.Local != wireOperation+"Request" || action == nil || action.get("op") != wireAction ||
						action.get("id") != "9" || action.get("l") != string(tt.target[2:]) || action.get("nci") != nci {
						t.Error("unexpected native write selector")
					}
					checkSOAPModifiedConstraint(t, r, strconv.FormatUint(uint64(tt.revision), 10))
					switch tt.outcome {
					case "transport loss":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					case "service.FAILURE", "service.PERM_DENIED", "mail.MODIFY_CONFLICT":
						fault(w, tt.outcome)
						return
					case "missing ID":
						created = ""
					}
					envelope(w, `<`+wireOperation+`Response xmlns="urn:zimbraMail"><action id="9" op="`+wireAction+`"`+created+`/></`+wireOperation+`Response>`)
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			ctx := t.Context()
			var got core.Object
			var err error
			switch tt.operation {
			case "move":
				got, err = box.MoveMailMessage(ctx, tt.source, tt.revision, tt.target)
			case "copy":
				got, err = box.CopyMailMessage(ctx, tt.source, tt.revision, tt.target)
			case "trash":
				got, err = box.TrashMailMessage(ctx, tt.source, tt.revision)
			case "restore":
				got, err = box.RestoreMailMessage(ctx, tt.source, tt.revision, tt.target)
			}
			if !errors.Is(err, tt.wantErr) || got.ID != tt.wantID {
				t.Fatalf("got ID %q and error %v, want %q and %v", got.ID, err, tt.wantID, tt.wantErr)
			}
			if count := actions.Load(); count > 1 || (count == 1) != tt.write {
				t.Fatalf("unexpected number of writes: %d", count)
			}
			if err == nil && (got.Parent != tt.target || got.Revision < tt.revision) {
				t.Fatal("missing verified parent and revision")
			}
		})
	}
}
