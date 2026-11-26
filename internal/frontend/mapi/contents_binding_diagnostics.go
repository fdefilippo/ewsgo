// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// Inspect all property-bearing arms of a completely decoded, rejected filter.
// Binding itself still returns its original first error. The existing schema
// resolver is pure; this scan never creates names or accesses the backend.
func recordContentsBindingDiagnostic(c contentsConfigure, registry *namedPropertyRegistry, d *StorePropertyDiagnostic) {
	if !d.unknown.enabled {
		return
	}
	supports := contentsProjectionSchema(c.frame.flags&0x20 != 0, c.frame.flags&0x10 != 0)
	nodes := 0
	var visit func(*contentsRestriction, int)
	visit = func(n *contentsRestriction, depth int) {
		if n == nil || depth > maxContentsRestrictionDepth || nodes >= maxContentsRestrictionNodes {
			return
		}
		nodes++
		if n.op == 3 || n.op == 4 || n.op == 6 || n.op == 8 {
			if _, err := resolveContentsProperty(n.tag, registry, supports); err != nil {
				if index := publicStorePropertyIndex(uint16(n.tag >> 16)); index >= 0 {
					d.propertyNames[index] = true
				} else {
					d.unknown.record(n.tag) // Same sixteen-tag budget as GetProperties.
				}
			}
		}
		for _, child := range n.children {
			visit(child, depth+1)
		}
	}
	visit(c.restriction, 1)
}
