# Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# $1 == 0 for uninstallation.
# $1 == 1 for removing old package during upgrade.

if [ $1 -eq 0 ] ; then
        # Package removal, not upgrade
        systemctl --no-reload disable tailscaled.service > /dev/null 2>&1 || :
        systemctl stop tailscaled.service > /dev/null 2>&1 || :
fi
