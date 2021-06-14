// getparty
// Copyright (C) 2016-2021 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/vbauerster/getparty"
)

var (
	commit = "xxxxxxx"
)

func main() {
	ctx, cancel := backgroundContext()
	defer cancel()
	cmd := &getparty.Cmd{
		Ctx: ctx,
		Out: os.Stdout,
		Err: os.Stderr,
	}
	os.Exit(cmd.Exit(cmd.Run(
		os.Args[1:],
		fmt.Sprintf("%s (%.7s) (%s)", getparty.Version, commit, runtime.Version()),
	)))
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
