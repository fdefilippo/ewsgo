// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestSetReceiveFolderPreservesCurrentAssociation(t *testing.T) {
	f := newOSTFixture(t)
	set := binary.LittleEndian.AppendUint64([]byte{ropSetReceiveFolder, 4, 0}, f.fids[MailboxRoot])
	set = append(append(set, "IPM.Note"...), 0)
	get := receiveFolderOperation(4, 0, "IPM.Note")
	current, result := f.run(get)
	if result.errors != 0 {
		t.Fatal("could not load current association")
	}
	out, result := f.run(append(bytes.Clone(set), get...))
	want := append(ropError(ropSetReceiveFolder, 0, NotSupported), current...)
	if !bytes.Equal(out, want) || result.errors != 1 {
		t.Fatalf("SetReceiveFolder followed by GetReceiveFolder = %x, want %x", out, want)
	}
}

func receiveFolderOperation(logon, input byte, class string) []byte {
	return append(append([]byte{0x27, logon, input}, class...), 0)
}

func TestReceiveFolderWireDefaultAssociations(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.ostPropertySpike = false
	for _, tc := range []struct {
		class, explicit string
		role            SpecialFolderRole
	}{
		{"", "", Inbox}, {"IPM.Note", "IPM", Inbox}, {"ipm", "IPM", Inbox},
		{"Report.IPM.Note.NDR", "Report.IPM", Inbox}, {"REPORT.IPC", "", Inbox},
		{"IPC", "IPC", MailboxRoot}, {"ipc.Test", "IPC", MailboxRoot},
		{"MY.Private.Class", "", Inbox}, {strings.Repeat("a", 254), "", Inbox},
	} {
		row, c := f.run(receiveFolderOperation(4, 0, tc.class))
		want := binary.LittleEndian.AppendUint64([]byte{0x27, 0, 0, 0, 0, 0}, f.fids[tc.role])
		want = append(append(want, tc.explicit...), 0)
		if !bytes.Equal(row, want) || c.errors != 0 || c.folders != 0 {
			t.Fatal("default association wire", tc.class)
		}
	}
	for _, class := range []string{".IPM", "IPM.", "IPM..Note", "a\nb", "\x7f", "é", strings.Repeat("a", 255)} {
		row, c := f.run(receiveFolderOperation(4, 0, class))
		if !bytes.Equal(row, []byte{0x27, 0, 0x57, 0, 7, 0x80}) || c.errors != 1 {
			t.Fatal("invalid class error")
		}
	}
}

func TestReceiveFolderFreshInboxRole(t *testing.T) {
	f := newOSTFixture(t)
	old := f.fids[Inbox]
	f.box.page.Objects[1].ID = "replacement-inbox"
	row, _ := f.run(receiveFolderOperation(4, 0, "IPM.Note"))
	if len(row) != 18 || binary.LittleEndian.Uint32(row[2:]) != 0 || binary.LittleEndian.Uint64(row[6:]) == old {
		t.Fatal("reused stale Inbox association")
	}
	fid := binary.LittleEndian.Uint64(row[6:])
	folder, err := f.state.mailbox.objects[f.handles[0]].binding.store.folder(context.Background(), f.session, fid)
	if err != nil || folder.native != "replacement-inbox" {
		t.Fatal("new Inbox not backed by current native role")
	}
	delete(f.box.page.Objects[1].Properties, core.WellKnown)
	row, _ = f.run(receiveFolderOperation(4, 0, ""))
	if len(row) != 6 || binary.LittleEndian.Uint32(row[2:]) != PropertyNotFound {
		t.Fatal("missing current Inbox invented")
	}
}

func TestReceiveFolderPermissionChecks(t *testing.T) {
	for _, name := range []string{"folder", "foreign-logon", "null", "released", "denied-root", "not-owner", "state-error", "foreign-scope", "denied-query", "cancelled", "ambiguous-inbox"} {
		t.Run(name, func(t *testing.T) {
			f := newOSTFixture(t)
			op := Operation{ID: 0x27, Logon: 4, Input: 0, MessageClass: "IPM.Note"}
			ctx := context.Background()
			session := f.session
			want := accessDenied
			switch name {
			case "folder":
				op.Input = 1
				want = NotSupported
			case "foreign-logon":
				op.Logon = 5
			case "null":
				f.handles[0] = 0xffffffff
				want = nullObject
			case "released":
				f.state.mailbox.release(f.handles[0])
				want = invalidObject
			case "denied-root":
				f.box.getErr = core.ErrAccessDenied
			case "not-owner":
				f.box.state.Owner = false
			case "state-error":
				f.box.stateErr = core.ErrAccessDenied
			case "foreign-scope":
				id := session.Identity()
				id.Scope.Account = "other"
				session = scopedSession{Session: session, identity: id}
			case "denied-query":
				f.box.err = core.ErrAccessDenied
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = 0x80004005
			case "ambiguous-inbox":
				f.box.page.Objects[2].Properties[core.WellKnown] = core.Text("inbox")
				want = storeError(core.ErrConflict)
			}
			row := getReceiveFolder(ctx, session, f.state.mailbox, op, f.handles)
			if len(row) != 6 || binary.LittleEndian.Uint32(row[2:]) != want {
				t.Fatal("permission rejection", name)
			}
		})
	}
}

func TestReceiveFolderPreflightAndMalformedBatchNoEffects(t *testing.T) {
	for _, suffix := range [][]byte{{0x27, 4, 0, 'I'}, {3, 4, 0}, {0x27, 4, 2, 0}} {
		f := newOSTFixture(t)
		before := f.box.calls
		body, _ := f.execute(append([]byte{1, 4, 1}, suffix...), 4096)
		if binary.LittleEndian.Uint32(body[4:]) == 0 || f.box.calls != before {
			t.Fatal("invalid batch executed read prefix")
		}
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
			t.Fatal("invalid batch released handle")
		}
	}
	f := newOSTFixture(t)
	before := f.box.calls
	body, _ := f.execute(append([]byte{1, 4, 1}, receiveFolderOperation(4, 0, "Report.IPM")...), 42)
	row := storeResponse(t, body)
	if row.Operations[0] != 0xff || binary.LittleEndian.Uint16(row.Operations[1:]) != 43 || f.box.calls != before {
		t.Fatal("preflight before effects")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("preflight released handle")
	}
	body, c := f.execute(receiveFolderOperation(4, 0, "Report.IPM"), 43)
	if len(storeResponse(t, body).Operations) != 25 || c.errors != 0 {
		t.Fatal("exact output bound")
	}
}

func FuzzReceiveFolderClass(f *testing.F) {
	f.Add([]byte("IPM.Note"))
	f.Add([]byte(""))
	f.Add([]byte("IPC..Invalid"))
	f.Fuzz(func(t *testing.T, value []byte) {
		if len(value) > 65500 || bytes.Contains(value, []byte{0}) {
			t.Skip()
		}
		ops, err := (ROPBuffer{Operations: receiveFolderOperation(4, 0, string(value)), Handles: []uint32{1}}).DecodeOperations()
		if err != nil || len(ops) != 1 {
			t.Fatal("framing")
		}
		op := ops[0]
		if op.InvalidMessageClass == validReceiveClass(value) || len(op.MessageClass) > 254 {
			t.Fatal("unbounded/invalid class")
		}
		if !op.InvalidMessageClass {
			_, explicit := receiveAssociation(op.MessageClass)
			if len(explicit) > 10 {
				t.Fatal("unbounded response")
			}
		}
	})
}
