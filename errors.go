//go:generate go tool stringer -type=mismatchType -trimprefix=mismatch
//go:generate go tool stringer -type=statusContextError -trimprefix=err

package getparty

import (
	"errors"
	"fmt"
	"runtime/debug"
)

const (
	mismatchContentType mismatchType = iota
	mismatchContentLength
)

const (
	errContextPartial statusContextError = iota
	errContextFallback
)

const (
	ErrCanceledByUser      = ExpectedError("Canceled by user")
	ErrMaxRedirect         = ExpectedError("Max redirections")
	ErrMaxRetry            = ExpectedError("Max retries")
	ErrZeroParts           = ExpectedError("No parts no work")
	ErrTooFragmented       = ExpectedError("Too fragmented, reduce number of parts maybe?")
	ErrInteractionRequired = ExpectedError("Interaction required, run without --queit maybe?")
	ErrMirrorStop          = ExpectedError("Stop, more than one mirror requested")
	ErrMirrorNone          = ExpectedError("None of the mirror responded with valid result")
)

var errSessionCanceled = errors.New("Session canceled")

type (
	mismatchType                int
	statusContextError          int
	UnexpectedHttpStatusError   int
	ExpectedError               string
	ContentMismatchError[T any] struct {
		kind mismatchType
		old  T
		new  T
	}
)

func (s statusContextError) Error() string {
	return s.String()
}

func (e ExpectedError) Error() string {
	return string(e)
}

func (e UnexpectedHttpStatusError) Error() string {
	return fmt.Sprintf("Unexpected http status: %d", int(e))
}

func (e ContentMismatchError[T]) Error() string {
	return fmt.Sprintf("%s mismatch: expected %v got %v", e.kind, e.old, e.new)
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
	if e, ok := errors.AsType[*debugError](err); ok {
		stack = append(stack, e.stack...)
	}
	return &debugError{
		error: err,
		stack: stack,
	}
}
