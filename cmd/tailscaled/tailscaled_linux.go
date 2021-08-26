// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"tailscale.com/chirp"
	"tailscale.com/wgengine"
)

func createBIRDClient(p string) (wgengine.BIRDClient, error) {
	return chirp.New(p)
}
