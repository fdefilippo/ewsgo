// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package memory implements the provisional contract for tests and early
// spikes. It is ephemeral, not a persistence layer or a real second backend.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"ewsgo/internal/core"
)

type Provider struct {
	mu    sync.Mutex
	boxes map[core.Scope]*mailbox
}

func New() *Provider { return &Provider{boxes: make(map[core.Scope]*mailbox)} }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (p *Provider) Open(ctx context.Context, scope core.Scope) (core.Mailbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scope.Account == "" || scope.Backend == "" {
		return nil, core.ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.boxes[scope]
	if b == nil {
		b = &mailbox{objects: make(map[core.ID]core.Object), generation: newID()}
		b.objects["root"] = core.Object{ID: "root", Kind: core.Folder, Revision: 1, Properties: core.Properties{}}
		p.boxes[scope] = b
	}
	return b, nil
}

type mailbox struct {
	mu         sync.Mutex
	objects    map[core.ID]core.Object
	generation string
	journal    []core.Change
}

func (*mailbox) Capabilities() core.Capabilities {
	return core.Capabilities{Write: true, Delete: true, Attachments: true, EqualQuery: true}
}
func (*mailbox) Root() core.ID { return "root" }
func clone(o core.Object) core.Object {
	o.Properties, _ = core.CloneProperties(o.Properties)
	o.MIME = append([]byte(nil), o.MIME...)
	o.Recipients = append([]core.Recipient(nil), o.Recipients...)
	o.Attachments = append([]core.Attachment(nil), o.Attachments...)
	for i := range o.Attachments {
		o.Attachments[i].Data = append([]byte(nil), o.Attachments[i].Data...)
	}
	return o
}
func (b *mailbox) Get(ctx context.Context, id core.ID) (core.Object, error) {
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[id]
	if !ok {
		return core.Object{}, core.ErrNotFound
	}
	return clone(o), nil
}
func limit(n int) (int, error) {
	if n == 0 {
		return 100, nil
	}
	if n < 0 || n > 1000 {
		return 0, core.ErrInvalid
	}
	return n, nil
}
func (b *mailbox) Query(ctx context.Context, q core.Query) (core.Page, error) {
	if err := ctx.Err(); err != nil {
		return core.Page{}, err
	}
	if q.OrderBy != nil {
		return core.Page{}, core.ErrUnsupported
	}
	n, err := limit(q.Limit)
	if err != nil || q.Offset < 0 || (q.Kind != 0 && q.Kind != core.Folder && q.Kind != core.Message) {
		return core.Page{}, core.ErrInvalid
	}
	for _, p := range q.Where {
		if _, err := core.CloneProperties(core.Properties{p.Key: p.Equal}); err != nil {
			return core.Page{}, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []core.Object
	for _, o := range b.objects {
		if o.Parent != q.Parent || (q.Kind != 0 && o.Kind != q.Kind) {
			continue
		}
		match := true
		for _, p := range q.Where {
			if !reflect.DeepEqual(o.Properties[p.Key], p.Equal) {
				match = false
				break
			}
		}
		if match {
			found = append(found, clone(o))
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
	start := min(q.Offset, len(found))
	end := start + min(n, len(found)-start)
	return core.Page{Objects: found[start:end], NextOffset: end, More: end < len(found)}, nil
}
func (b *mailbox) Begin(ctx context.Context, target core.Target) (core.Edit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var object core.Object
	if target.ID == "" {
		if (target.Kind != core.Folder && target.Kind != core.Message) || target.Expected != 0 {
			return nil, core.ErrInvalid
		}
		object = core.Object{ID: core.ID(newID()), Kind: target.Kind, Parent: target.Parent, Properties: core.Properties{}}
	} else {
		var ok bool
		object, ok = b.objects[target.ID]
		if !ok {
			return nil, core.ErrNotFound
		}
		if target.Expected == 0 || target.Expected != object.Revision {
			return nil, core.ErrConflict
		}
	}
	if object.ID == b.Root() {
		return nil, core.ErrUnsupported
	}
	return &edit{box: b, object: clone(object), expected: target.Expected}, nil
}

type edit struct {
	mu       sync.Mutex
	box      *mailbox
	object   core.Object
	expected core.Revision
	closed   bool
}

func (e *edit) Apply(p core.Patch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return core.ErrClosed
	}
	set, err := core.CloneProperties(p.Set)
	if err != nil {
		return err
	}
	for _, k := range p.Remove {
		if !k.Valid() {
			return core.ErrInvalid
		}
		if _, ok := set[k]; ok {
			return core.ErrInvalid
		}
	}
	for k, v := range set {
		if old, ok := e.object.Properties[k]; ok && !core.SameType(old, v) {
			return core.ErrInvalid
		}
	}
	o := clone(e.object)
	for k, v := range set {
		o.Properties[k] = v
	}
	for _, k := range p.Remove {
		delete(o.Properties, k)
	}
	if p.Parent != nil {
		o.Parent = *p.Parent
	}
	if p.MIME != nil {
		o.MIME = append([]byte(nil), (*p.MIME)...)
	}
	if p.Recipients != nil {
		o.Recipients = append([]core.Recipient(nil), (*p.Recipients)...)
	}
	if p.Attachments != nil {
		o.Attachments = append([]core.Attachment(nil), (*p.Attachments)...)
	}
	if o.Kind == core.Folder && (len(o.MIME) > 0 || len(o.Recipients) > 0 || len(o.Attachments) > 0) {
		return core.ErrInvalid
	}
	e.object = clone(o)
	return nil
}
func (e *edit) Commit(ctx context.Context) (core.Object, error) {
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return core.Object{}, core.ErrClosed
	}
	b := e.box
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.objects[e.object.ID]
	if (e.expected == 0 && exists) || (e.expected != 0 && (!exists || current.Revision != e.expected)) {
		return core.Object{}, core.ErrConflict
	}
	parent, ok := b.objects[e.object.Parent]
	if !ok || parent.Kind != core.Folder {
		return core.Object{}, core.ErrInvalid
	}
	// Reject moves that would make a folder its own ancestor.
	for id := e.object.Parent; id != ""; id = b.objects[id].Parent {
		if id == e.object.ID {
			return core.Object{}, core.ErrInvalid
		}
	}
	o := clone(e.object)
	o.Revision = current.Revision + 1
	kind := core.Updated
	if !exists {
		kind = core.Created
	} else if current.Parent != o.Parent {
		kind = core.Moved
	}
	b.objects[o.ID] = o
	b.journal = append(b.journal, core.Change{Kind: kind, ID: o.ID, Parent: o.Parent, PreviousParent: current.Parent, Revision: o.Revision})
	e.closed = true
	return clone(o), nil
}
func (e *edit) Discard() { e.mu.Lock(); defer e.mu.Unlock(); e.closed = true }
func (b *mailbox) Delete(ctx context.Context, id core.ID, expected core.Revision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[id]
	if !ok {
		return core.ErrNotFound
	}
	if id == b.Root() {
		return core.ErrUnsupported
	}
	if expected == 0 || o.Revision != expected {
		return core.ErrConflict
	}
	for _, other := range b.objects {
		if other.Parent == id {
			return core.ErrConflict
		}
	}
	delete(b.objects, id)
	b.journal = append(b.journal, core.Change{Kind: core.Deleted, ID: id, Parent: o.Parent, PreviousParent: o.Parent, Revision: o.Revision + 1})
	return nil
}
func (b *mailbox) Changes(ctx context.Context, after core.Cursor, maxEvents int) (core.Changes, error) {
	if err := ctx.Err(); err != nil {
		return core.Changes{}, err
	}
	n, err := limit(maxEvents)
	if err != nil {
		return core.Changes{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	offset := 0
	if after != "" {
		s := strings.Split(string(after), "/")
		if len(s) != 2 || s[0] != b.generation {
			return core.Changes{}, core.ErrResync
		}
		offset, err = strconv.Atoi(s[1])
		if err != nil || offset < 0 || offset > len(b.journal) {
			return core.Changes{}, core.ErrResync
		}
	}
	end := offset + min(n, len(b.journal)-offset)
	return core.Changes{Events: append([]core.Change(nil), b.journal[offset:end]...), Next: core.Cursor(b.generation + "/" + strconv.Itoa(end)), More: end < len(b.journal)}, nil
}

var _ core.Provider = (*Provider)(nil)
var _ core.Mailbox = (*mailbox)(nil)
var _ core.Edit = (*edit)(nil)
