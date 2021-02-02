#!/usr/bin/env sh
# Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -eu

exec >&2

if [ $# -ne 1 ]; then
	echo "Usage: $0 <package output dir> <goarch> deb|rpm <distro name>"
	exit 1
fi
OUT="$1"

archs="amd64 386 arm arm64"

for distro in `cat packaging/deb_distros`; do
	for arch in $archs; do
		./build_linux_package.sh "$OUT" "$arch" deb "$distro" &
	done
done
for distro in `cat packaging/rpm_distros`; do
	for arch in $archs; do
		./build_linux_package.sh "$OUT" "$arch" rpm "$distro" &
	done
done

wait
