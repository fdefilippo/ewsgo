#!/bin/sh
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu

extension_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
zimbra_root=${ZIMBRA_ROOT:-/opt/zimbra}
jdk=${ZIMBRA_JDK:-"$zimbra_root/common/lib/jvm/java/bin"}
out=${1:?usage: build.sh output.jar}
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT HUP INT TERM

"$jdk/javac" --release 17 -cp "$zimbra_root/lib/jars/*" -d "$build/classes" \
    "$extension_dir"/src/org/ewsgo/zimbra/*.java
printf 'Manifest-Version: 1.0\nZimbra-Extension-Class: org.ewsgo.zimbra.IncomingRulesExtension\n' > "$build/manifest"
"$jdk/jar" --create --file "$out" --manifest "$build/manifest" -C "$build/classes" .
