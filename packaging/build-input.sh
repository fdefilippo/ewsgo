#!/bin/sh
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu
output=${1:?Specify a build output directory}
mkdir -p "$output/licenses/Go"

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOTOOLCHAIN=local
go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o "$output/ewsgo" ./cmd/ewsgo
cp -f LICENSE README.md README.it.md "$output/"
cp -rf assets docs "$output/"

goroot=$(go env GOROOT)
cp -f "$goroot/LICENSE" "$goroot/PATENTS" "$output/licenses/Go/"

go list -mod=readonly -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' ./cmd/ewsgo > "$output/modules"
sort -u "$output/modules" | while IFS='|' read -r module version directory; do
    [ -n "$module" ] || continue
    notices="$output/licenses/$module@$version"
    mkdir -p "$notices"
    found_license=false
    for file in "$directory"/LICENSE* "$directory"/COPYING* "$directory"/NOTICE* \
        "$directory"/PATENTS* "$directory"/AUTHORS*; do
        [ -f "$file" ] || continue
        cp -f "$file" "$notices/"
        case ${file##*/} in
            LICENSE*|COPYING*) found_license=true ;;
        esac
    done
    if ! "$found_license"; then
        printf 'No license file found for %s@%s\n' "$module" "$version" >&2
        exit 1
    fi
done
rm -f "$output/modules"
