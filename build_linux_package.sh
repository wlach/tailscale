#!/usr/bin/env sh
# Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -eu

exec >&2

if [ $# -ne 4 ]; then
	echo "Usage: $0 <package output dir> <goarch> deb|rpm <distro name>"
	exit 1
fi
OUT="$1"
PKGARCH="$2"
PKGTYPE="$3"
DISTRO="$4"

if [ "$(go env GOOS)" != "linux" ]; then
	echo "I only know how to build packages for linux, please set GOOS=linux"
	exit 1
fi

eval $(./version/version.sh)

mkdir -p "$OUT"
tmp=$(mktemp -d -p "$OUT")
cleanup() {
	rm -rf "$tmp"
}
trap cleanup EXIT

echo "Building tailscale binaries for $PKGARCH"
GOARCH="$PKGARCH" ./build_dist.sh -o "$tmp" ./cmd/tailscale ./cmd/tailscaled

echo "Staging package files for $DISTRO/$PKGARCH"
find_file() {
	src="packaging/$DISTRO/$1"
	if [ -f "$src" ]; then
		echo "$src"
	else
		echo "packaging/$1"
	fi
}
staging="$tmp/staging"
install -m755 -d -- "$staging"
install -m755 -- "$tmp/tailscale" "$tmp/tailscaled" "$staging"
for f in postinst prerm postrm; do
	install -m755 -- "$(find_file $PKGTYPE.$f.sh)" "$staging"
done
for f in service defaults; do
	install -m644 -- "$(find_file tailscaled.$f)" "$staging"
done

echo "Building package for $DISTRO/$PKGARCH"
mkdir -p "$OUT/$DISTRO"
GOARCH="" go run ./cmd/mkpkg \
	  -o "$OUT/$DISTRO" \
	  -t "$PKGTYPE" \
	  -a "$PKGARCH" \
	  --version "$VERSION_SHORT" \
	  --files="${staging}/tailscaled.service:/lib/systemd/system/tailscaled.service,${staging}/tailscale:/usr/bin/tailscale,${staging}/tailscaled:/usr/sbin/tailscaled" \
	  --configs="${staging}/tailscaled.defaults:/etc/default/tailscaled" \
	  --postinst="${staging}/${PKGTYPE}.postinst.sh" \
	  --prerm="${staging}/${PKGTYPE}.prerm.sh" \
	  --postrm="${staging}/${PKGTYPE}.postrm.sh" \
	  --replaces "tailscale-relay" \
	  --depends "iptables,iproute2"
