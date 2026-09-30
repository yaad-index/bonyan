package model

import (
	"errors"
	"fmt"
)

// ErrMissingUsage reports that a model returned no usage where usage is
// required, as in a budgeted run.
var ErrMissingUsage = errors.New("model: no usage reported")

// ErrorKind classifies a failed call.
type ErrorKind string

// The kinds of call failure.
const (
	ErrTimeout   ErrorKind = "timeout"   // the call's deadline passed
	ErrCanceled  ErrorKind = "canceled"  // the caller canceled
	ErrRejected  ErrorKind = "rejected"  // the endpoint refused the request
	ErrTransport ErrorKind = "transport" // the request did not complete
	ErrInvalid   ErrorKind = "invalid"   // the reply could not be understood
)

// CallError is a failed model call.
type CallError struct {
	Kind      ErrorKind
	Retryable bool
	// NotSent is set by an adapter that knows the request never reached the
	// endpoint (the connection was refused, the name did not resolve). The
	// zero value means it may have been sent, so a budgeted run charges the
	// call's bound (ADR 0001 §11); an adapter sets NotSent only when it is sure.
	NotSent bool
	Err     error
}

func (e *CallError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("model call failed (%s)", e.Kind)
	}
	return fmt.Sprintf("model call failed (%s): %v", e.Kind, e.Err)
}

func (e *CallError) Unwrap() error { return e.Err }
