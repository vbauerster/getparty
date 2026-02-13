// getparty
// Copyright (C) 2016-2025 Vladimir Bauer
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
	quit := make(chan os.Signal, 2)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer func() {
		cancel(nil)
		signal.Stop(quit)
		os.Exit(status)
	}()
	go func() {
		select {
		case <-quit:
			cancel(getparty.ErrCanceledByUser)
		case <-ctx.Done():
		}
	}()
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	cmd := &getparty.Cmd{
		Ctx: ctx,
		Out: os.Stdout,
		Err: os.Stderr,
	}
	status = cmd.Exit(cmd.Run(os.Args[1:], version, commit))
}
