package getparty

import (
	"errors"
	"fmt"
	"runtime/debug"
)

type (
	UnexpectedHttpStatus int
	ExpectedError        string
	ContentMismatch      struct {
		expected int64
		got      int64
	}
	BadProxyURL struct {
		err error
	}
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

func (e BadProxyURL) Error() string {
	return fmt.Sprintf("Bad proxy url: %s", e.err.Error())
}

func (e BadProxyURL) Unwrap() error {
	return e.err
}

type debugError struct {
	error
	stack []byte
}

func (s *debugError) Unwrap() error { return s.error }

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
	return &debugError{
		err,
		debug.Stack(),
	}
}
