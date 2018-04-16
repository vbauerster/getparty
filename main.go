// getparty
// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"os"

	"github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/getparty/gp"
)

var version = "devel"

func main() {
	cmd := &gp.Cmd{Out: os.Stdout, Err: os.Stderr}
	help, err := cmd.Run(os.Args[1:], version)
	if err == nil {
		os.Exit(0)
	}
	switch err := errors.Cause(err).(type) {
	case *flags.Error:
		if err.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			help()
			os.Exit(2)
		}
	case gp.Error:
		help()
		os.Exit(1)
	default:
		log.Fatalf("unexpected error: %v\n", err)
	}
}
