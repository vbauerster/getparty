// getparty
// Copyright (C) 2016-2024 Vladimir Bauer
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
	commit  = "xxxxxxx"
)

func main() {
	runtime.MemProfileRate = 0
	ctx, cancel := backgroundContext()
	defer cancel()
	cmd := &getparty.Cmd{
		Ctx: ctx,
		Out: os.Stdout,
		Err: os.Stderr,
	}
	os.Exit(cmd.Exit(cmd.Run(os.Args[1:], version, commit)))
}

func backgroundContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(quit)
		<-quit
		cancel()
	}()

	return ctx, cancel
}
