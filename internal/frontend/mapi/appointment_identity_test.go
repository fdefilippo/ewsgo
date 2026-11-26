// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/hex"
	"testing"
)

func TestAppointmentGlobalIdentityKeepsImportedMeetingIdentity(t *testing.T) {
	const imported = "040000008200e00074c5b7101a82e00807d8051c3046642b576ac801000000000000000010000000622c639e40d09342b747a1672730cbba"
	const master = "040000008200e00074c5b7101a82e008000000003046642b576ac801000000000000000010000000622c639e40d09342b747a1672730cbba"
	if got := hex.EncodeToString(appointmentGlobalIdentity(imported)); got != master {
		t.Fatal("imported appointment lost the meeting identifier or retained the occurrence date")
	}
}
