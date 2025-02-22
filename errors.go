package getparty

import (
	"errors"
	"fmt"
	"runtime/debug"
)

type (
	ExpectedError        string
	UnexpectedHttpStatus int
	ContentMismatch      struct {
		expected int64
		got      int64
	}
	stack struct {
		error
		stack []byte
	}
	singleModeFallback int
)

func (e ExpectedError) Error() string {
	return string(e)
}

func (e UnexpectedHttpStatus) Error() string {
	return fmt.Sprintf("Unexpected http status: %d", int(e))
}

func (e ContentMismatch) Error() string {
	return fmt.Sprintf("ContentLength mismatch: expected %d got %d", e.expected, e.got)
}

func (e singleModeFallback) Error() string {
	return fmt.Sprintf("P%02d: fallback to single mode", int(e))
}

func (s *stack) Unwrap() error { return s.error }

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
