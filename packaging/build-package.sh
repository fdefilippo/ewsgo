#!/bin/sh
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

set -eu
if [ "$#" -ne 4 ]; then
    printf 'Usage: %s TARGET VERSION INPUT_DIR OUTPUT_DIR\n' "$0" >&2
    exit 2
fi

target=$1
version=$2
input=$(cd "$3" && pwd)
output=$(cd "$4" && pwd)
packaging=$(cd "$(dirname "$0")" && pwd)
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(~[a-z0-9]+(\.[a-z0-9]+)*)?$'; then
    printf 'Invalid package version: %s\n' "$version" >&2
    exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck disable=SC1091
. /etc/os-release
package_doc=/usr/share/doc/ewsgo
case "$target:$ID:$VERSION_ID" in
    el8:rocky:8.*|el9:rocky:9.*|el10:rocky:10.*)
        dnf -y install rpm-build
        rpmbuild -bb --target x86_64 \
            --define "_topdir $work" \
            --define "input_dir $input" \
            --define "package_version $version" \
            --define "dist .$target" \
            "$packaging/ewsgo.spec"
        package=$(find "$work/RPMS" -type f -name 'ewsgo-*.rpm')
        dnf -y --setopt=tsflags= install "$package"
        rpm -q ewsgo
        ;;
    debian13:debian:13|ubuntu22.04:ubuntu:22.04|ubuntu24.04:ubuntu:24.04|ubuntu26.04:ubuntu:26.04)
        root="$work/root"
        doc="$root/usr/share/doc/ewsgo"
        install -d "$root/DEBIAN" "$doc"
        install -Dm755 "$input/ewsgo" "$root/usr/bin/ewsgo"
        install -m644 "$input/README.md" "$input/README.it.md" "$doc/"
        install -m644 "$input/LICENSE" "$doc/LICENSE"
        cp -rf "$input/assets" "$input/docs" "$doc/"
        cp -r "$input/licenses" "$doc/third-party"
        {
            printf 'Copyright: 2026 Defilippo srls info@defilippo.org\nLicense: AGPL-3.0-or-later\n\n'
            cat "$input/LICENSE"
        } > "$doc/copyright"
        debversion="$version-1+$target"
        cat > "$root/DEBIAN/control" <<EOF
Package: ewsgo
Version: $debversion
Section: mail
Priority: optional
Architecture: amd64
Maintainer: Defilippo srls <info@defilippo.org>
Depends: ca-certificates
Description: EWS and MAPI over HTTP gateway for mail servers
 EWSGo connects compatible clients to a configurable mail backend.
 The first backend uses the native Zimbra SOAP and REST interfaces.
EOF
        package="$work/ewsgo_${debversion}_amd64.deb"
        dpkg-deb --root-owner-group --build "$root" "$package"
        # Some base images exclude documentation during installation.
        dpkg-deb --extract "$package" "$work/unpacked"
        package_doc="$work/unpacked$package_doc"
        set --
        if [ "$target" = ubuntu22.04 ]; then
            # The package and its dependencies need only the main component.
            cat > "$work/jammy-main.list" <<'EOF'
deb http://archive.ubuntu.com/ubuntu/ jammy main
deb http://archive.ubuntu.com/ubuntu/ jammy-updates main
deb http://security.ubuntu.com/ubuntu/ jammy-security main
EOF
            set -- -o "Dir::Etc::sourcelist=$work/jammy-main.list" -o Dir::Etc::sourceparts=-
        fi
        apt-get "$@" update
        DEBIAN_FRONTEND=noninteractive apt-get "$@" install -y --no-install-recommends "$package"
        dpkg-query -W ewsgo
        ;;
    *)
        printf 'Target %s does not match this distribution (%s %s)\n' "$target" "$ID" "$VERSION_ID" >&2
        exit 2
        ;;
esac

/usr/bin/ewsgo -help > "$work/help" 2>&1
grep -q -- '-zimbra-origin' "$work/help"
cmp "$package_doc/LICENSE" "$input/LICENSE"
cmp "$package_doc/README.md" "$input/README.md"
cmp "$package_doc/README.it.md" "$input/README.it.md"
cmp "$package_doc/assets/ewsgo-logo.png" "$input/assets/ewsgo-logo.png"
cmp "$package_doc/docs/zimbra.md" "$input/docs/zimbra.md"
cp -f "$package" "$output/"
cd "$output"
sha256sum "${package##*/}" > SHA256SUMS
