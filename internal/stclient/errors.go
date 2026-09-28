package stclient

import (
	"errors"
	"fmt"
)

// ErrKind classifies a failed Syncthing API call.
type ErrKind int

const (
	// ErrUnreachable: the connection was refused, timed out or broke.
	ErrUnreachable ErrKind = iota
	// ErrUnauthorized: Syncthing answered 401 or 403 (the API key was rejected).
	ErrUnauthorized
	// ErrBadResponse: an unexpected HTTP status or a body that is not the expected JSON.
	ErrBadResponse
)

func (k ErrKind) String() string {
	switch k {
	case ErrUnreachable:
		return "unreachable"
	case ErrUnauthorized:
		return "unauthorized"
	case ErrBadResponse:
		return "bad response"
	}
	return fmt.Sprintf("ErrKind(%d)", int(k))
}

// Error is the only error type returned by Client methods.
// Status is the HTTP status code, or 0 when no response was received.
type Error struct {
	Kind   ErrKind
	Status int
	Err    error
}

func (e *Error) Error() string {
	msg := e.Summary()
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Summary is a short, user-facing description without the underlying cause,
// suitable for activity notes ("Command failed: <summary>").
func (e *Error) Summary() string {
	switch e.Kind {
	case ErrUnreachable:
		return "Syncthing is not responding"
	case ErrUnauthorized:
		return "Syncthing rejected the API key"
	default:
		if e.Status != 0 && (e.Status < 200 || e.Status > 299) {
			return fmt.Sprintf("unexpected response from Syncthing (HTTP %d)", e.Status)
		}
		return "unexpected response from Syncthing"
	}
}

// KindOf reports the ErrKind of err if err is (or wraps) an *Error.
func KindOf(err error) (ErrKind, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind, true
	}
	return 0, false
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}
