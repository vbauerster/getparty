//go:generate go tool stringer -type=mismatchType -trimprefix=mismatch

package getparty

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
)

const (
	mismatchContentType mismatchType = iota
	mismatchContentLength
)

const (
	errContextPartial  = sessionFirstHttpStatus(http.StatusPartialContent)
	errContextFallback = sessionFirstHttpStatus(http.StatusOK)
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

// errSessionCanceled indicates that session canceled due to unexpected error.
// Target usage is context.Cause check.
var errSessionCanceled = errors.New("session canceled")

// errTimeout indicates context deadline due to timeout.
// Target usage is context.Cause check.
var errTimeout = errors.New("context deadline exceeded")

type (
	mismatchType                int
	sessionFirstHttpStatus      int
	UnexpectedHttpStatusError   int
	ExpectedError               string
	ContentMismatchError[T any] struct {
		kind mismatchType
		old  T
		new  T
	}
)

func (s sessionFirstHttpStatus) Error() string {
	return strconv.Itoa(int(s))
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
