package getparty

import (
	"errors"
	"fmt"
	"runtime/debug"
)

type (
	UnexpectedHttpStatus   int
	ExpectedError          string
	ContentMismatch[T any] struct {
		kind string
		old  T
		new  T
	}
	BadProxyURL struct {
		error
	}
)

func (e ExpectedError) Error() string {
	return string(e)
}

func (e UnexpectedHttpStatus) Error() string {
	return fmt.Sprintf("Unexpected http status: %d", int(e))
}

func (e ContentMismatch[T]) Error() string {
	return fmt.Sprintf("Content%s mismatch: expected %v got %v", e.kind, e.old, e.new)
}

func (e BadProxyURL) Unwrap() error {
	return e.error
}

type debugError struct {
	error
	stack []byte
}

func (e *debugError) Unwrap() error {
	return e.error
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
	stack := debug.Stack()
	if e := (*debugError)(nil); errors.As(err, &e) {
		stack = append(stack, e.stack...)
	}
	return &debugError{
		err,
		stack,
	}
}
