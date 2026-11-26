#!/bin/sh
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

# Provider-neutral CI entry point; no credentials or live services required.
set -eu
cd "$(dirname "$0")/.."
exec make check
