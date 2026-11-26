// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

func TestSoftDeleteHonorsNonReadReceiptOption(t *testing.T) {
	tests := []struct {
		name, flags, content string
		wantErr              error
	}{
		{name: "unread plain mail", flags: "u", content: `<mp ct="text/plain"/>`},
		{name: "already read", content: `<header n="Disposition-Notification-To">sender@example.test</header>`},
		{name: "receipt header", flags: "u", content: `<header n="disposition-notification-to"/><mp ct="text/plain"/>`, wantErr: core.ErrUnsupported},
		{name: "receipt address", flags: "u", content: `<e t="n" a="sender@example.test"/><mp ct="text/plain"/>`, wantErr: core.ErrUnsupported},
		{name: "opaque attachment", flags: "u", content: `<mp ct="multipart/mixed"><mp ct="text/plain"/><mp ct="application/ms-tnef"/></mp>`, wantErr: core.ErrUnsupported},
		{name: "no MIME metadata", flags: "u", wantErr: core.ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deleted atomic.Bool
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				req := readMessageRequest(t, r)
				switch req.XMLName.Local {
				case "GetMsgRequest":
					if deleted.Load() {
						fault(w, "mail.NO_SUCH_MSG")
						return
					}
					selector := req.child(mailNS, "m")
					if selector == nil || selector.get("id") != "9" || selector.get("read") != "0" ||
						selector.get("wantModSeq") != "1" || selector.child(mailNS, "header") == nil ||
						selector.child(mailNS, "header").get("n") != dispositionNotificationTo {
						t.Error("unexpected receipt lookup")
					}
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="3" ms="7" rev="2" f="`+tt.flags+`">`+tt.content+`</m></GetMsgResponse>`)
				case "GetFolderRequest":
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="root"><folder id="3" l="1" name="Trash"/></folder></GetFolderResponse>`)
				case "GetInfoRequest":
					envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><attrs><attr name="zimbraDumpsterEnabled">TRUE</attr></attrs></GetInfoResponse>`)
				case "MsgActionRequest":
					action := req.child(mailNS, "action")
					if action == nil || action.get("id") != "9" || action.get("op") != "delete" || deleted.Swap(true) {
						t.Error("unexpected native delete")
					}
					checkSOAPModifiedConstraint(t, r, "7")
					envelope(w, `<MsgActionResponse xmlns="urn:zimbraMail"><action id="9" op="delete"/></MsgActionResponse>`)
				case "SearchRequest":
					if req.get("inDumpster") != "1" || !deleted.Load() {
						t.Error("unexpected recoverable lookup")
					}
					envelope(w, `<SearchResponse xmlns="urn:zimbraMail" more="0"><m id="9" l="3"/></SearchResponse>`)
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			err := box.SoftDeleteMailMessageWithNonReadReceipt(t.Context(), "m:9", 7)
			if !errors.Is(err, tt.wantErr) || deleted.Load() != (tt.wantErr == nil) {
				t.Fatalf("delete returned %v, deleted=%t, want %v", err, deleted.Load(), tt.wantErr)
			}
		})
	}
}
