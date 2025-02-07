package getparty

import (
	"errors"
	"fmt"
	"runtime/debug"
)

func firstErr(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func unwrapOrErr(err error) error {
	if e := errors.Unwrap(err); e != nil {
		return e
	}
	return err
}

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
