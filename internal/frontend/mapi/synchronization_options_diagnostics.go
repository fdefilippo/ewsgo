// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// SynchronizationOptionsDiagnostic retains protocol options only, never any
// tag, restriction body, identity, handle or folder ID. Formatting is redacted;
// Fields exposes only a closed vocabulary, including unknown-bit indicators.
type SynchronizationOptionsDiagnostic struct {
	contentsProfile      bool
	commitNonfinal       bool
	importRejection      importRejection
	messageImportRefusal importRejection
	hierarchyRefusal     hierarchyImportRefusal
	restrictionRejection contentsRestrictionRejection
	configureFailure     contentsConfigureFailure
	uploadFailure        contentsUploadFailure
	transferStateFailure transferStateFailure
	parsed               bool
	kind, send           byte
	flags                uint16
	extra                uint32
	restriction          bool
	tagBucket            byte
}

func (d SynchronizationOptionsDiagnostic) appendExecutionRejections(rejections []string) []string {
	rejections = append(rejections, d.uploadFailure.labels()...)
	rejections = d.transferStateFailure.appendLabels(rejections)
	if d.hierarchyRefusal != 0 {
		rejections = append(rejections, d.hierarchyRefusal.label())
	}
	if d.messageImportRefusal != 0 {
		rejections = append(rejections, d.messageImportRefusal.label())
	}
	return rejections
}

func (SynchronizationOptionsDiagnostic) String() string {
	return "SynchronizationOptionsDiagnostic{redacted}"
}
func (SynchronizationOptionsDiagnostic) GoString() string {
	return "SynchronizationOptionsDiagnostic{redacted}"
}

func inspectSynchronizationOptions(c synchronizationConfigure) SynchronizationOptionsDiagnostic {
	d := SynchronizationOptionsDiagnostic{parsed: true, kind: c.kind, send: c.sendOptions, flags: c.flags, extra: c.extraFlags, restriction: c.restrictionBytes != 0}
	if len(c.tags) > maxFolderPropertyTags {
		d.tagBucket = 3
	} else if len(c.tags) > 16 {
		d.tagBucket = 2
	} else if len(c.tags) > 0 {
		d.tagBucket = 1
	}
	return d
}

func inspectContentsOptions(c synchronizationConfigure) SynchronizationOptionsDiagnostic {
	d := inspectSynchronizationOptions(c)
	d.contentsProfile = true
	return d
}

type optionName struct {
	mask uint32
	name string
}

func optionNames(bits uint32, names []optionName) []string {
	var out []string
	var known uint32
	for _, item := range names {
		known |= item.mask
		if bits&item.mask != 0 {
			out = append(out, item.name)
		}
	}
	if bits & ^known != 0 {
		out = append(out, "unknown_bits")
	}
	if len(out) == 0 {
		return []string{"empty"}
	}
	return out
}

// Fields cannot expose raw client numbers or text, even for unknown bits.
func (d SynchronizationOptionsDiagnostic) Fields() (flags, send, extra, rejections []string, restriction, tags string) {
	if d.commitNonfinal {
		// A batch policy rejection, not Configure options or client data.
		return []string{"none"}, []string{"none"}, []string{"none"}, []string{"commit_nonfinal_batch"}, "none", "none"
	}
	if d.importRejection != 0 {
		// The shared rejection field also describes import parser failures.
		// None of these fields should imply that Configure was parsed.
		return []string{"none"}, []string{"none"}, []string{"none"}, []string{d.importRejection.label()}, "none", "none"
	}
	if !d.parsed {
		rejections = d.appendExecutionRejections(rejections)
		if len(rejections) == 0 {
			rejections = []string{"none"}
		}
		return []string{"none"}, []string{"none"}, []string{"none"}, rejections, "none", "none"
	}
	flags = optionNames(uint32(d.flags), []optionName{{1, "Unicode"}, {2, "NoDeletions"}, {4, "IgnoreNoLongerInScope"}, {8, "ReadState"}, {16, "FAI"}, {32, "Normal"}, {128, "OnlySpecifiedProperties"}, {256, "NoForeignIdentifiers"}, {4096, "Reserved"}, {8192, "BestBody"}, {16384, "IgnoreSpecifiedOnFAI"}, {32768, "Progress"}})
	send = optionNames(uint32(d.send), []optionName{{1, "Unicode"}, {2, "UseCpid"}, {4, "RecoverMode"}, {8, "ForceUnicode"}, {16, "PartialItem"}, {32, "Reserved1"}, {64, "Reserved2"}})
	extra = optionNames(d.extra, []optionName{{1, "Eid"}, {2, "MessageSize"}, {4, "CN"}, {8, "OrderByDeliveryTime"}})
	restriction = "absent"
	if d.restriction {
		restriction = "present"
	}
	tags = "zero"
	if d.tagBucket == 1 {
		tags = "1_16"
	} else if d.tagBucket == 2 {
		tags = "17_256"
	} else if d.tagBucket == 3 {
		tags = "over_256"
	}
	flagsMask, sendMask := uint16(0x0101), byte(0x69)
	kind := byte(2)
	if d.contentsProfile {
		flagsMask, sendMask, kind = 0xe1bf, 0x7d, 1
	}
	if d.kind != kind {
		rejections = append(rejections, "synchronization_type")
	}
	if d.flags&1 == 0 {
		rejections = append(rejections, "sync_unicode_required")
	}
	if d.flags & ^flagsMask != 0 {
		rejections = append(rejections, "sync_flags")
	}
	if d.send&1 == 0 {
		rejections = append(rejections, "send_unicode_required")
	}
	if d.send & ^sendMask != 0 {
		rejections = append(rejections, "send_options")
	}
	if !d.contentsProfile && d.extra&0x0e != 0 {
		rejections = append(rejections, "extra_flags")
	}
	if d.restriction && !d.contentsProfile {
		rejections = append(rejections, "restriction")
	}
	if d.restrictionRejection != 0 {
		rejections = append(rejections, d.restrictionRejection.label())
	}
	if d.tagBucket == 3 {
		rejections = append(rejections, "property_tag_limit")
	}
	rejections = append(rejections, d.configureFailure.labels()...)
	rejections = d.appendExecutionRejections(rejections)
	if len(rejections) == 0 {
		rejections = []string{"empty"}
	}
	return
}
