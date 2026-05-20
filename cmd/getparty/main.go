// getparty
// Copyright (C) 2016-2026 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/vbauerster/getparty"
)

var (
	version = "dev"
	commit  = "HEAD"
)

func main() {
	runtime.MemProfileRate = 0
	var status int
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		stop()
		os.Exit(status)
	}()
	cmd := &getparty.Cmd{
		Ctx: ctx,
		Out: os.Stdout,
		Err: os.Stderr,
	}
	status = cmd.Exit(cmd.Run(os.Args[1:], version, commit))
}
