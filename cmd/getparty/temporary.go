package main

import "github.com/pkg/errors"

type temporary interface {
	Temporary() bool
}

func isTemporary(err error) bool {
	te, ok := errors.Cause(err).(temporary)
	return ok && te.Temporary()
}
