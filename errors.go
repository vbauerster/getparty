package getparty

import (
	"fmt"
	"runtime/debug"
)

func withMessage(err error, message string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func withStack(err error) error {
	if err == nil {
		return nil
	}
	return &stack{
		err,
		debug.Stack(),
	}
}

type stack struct {
	error
	stack []byte
}

func (w *stack) Unwrap() error { return w.error }
