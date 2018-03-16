// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

func isTemporary(err error) bool {
	t, ok := err.(interface{ Temporary() bool })
	return ok && t.Temporary()
}
