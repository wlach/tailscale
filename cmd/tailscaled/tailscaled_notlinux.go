// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux
// +build !linux

package main // import "tailscale.com/cmd/tailscaled"

import (
	"tailscale.com/wgengine"
)

func createBIRDClient(_ string) (wgengine.BIRDClient, error) {
	return nil, nil
}
