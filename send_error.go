package cf_resend

import (
	"errors"
	"fmt"
)

// SendError is returned by Send when the Resend HTTP call failed. The SDK
// error string does not carry a status code; HTTPStatus is filled from the
// component transport (the same source as resend_emails_failed_total).
//
// Status 0 means a transport/network failure or an unknown status. Client()
// bypass (Emails.Send without going through Send) is not wrapped.
type SendError struct {
	err    error
	status int
}

func (e *SendError) Error() string {
	if e == nil {
		return ""
	}
	if e.status > 0 {
		return fmt.Sprintf("cf_resend: send failed (HTTP %d): %v", e.status, e.err)
	}
	return fmt.Sprintf("cf_resend: send failed: %v", e.err)
}

// Unwrap returns the SDK (or transport) error so errors.Is / errors.As keep
// working on the inner value.
func (e *SendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// HTTPStatus is the Resend API status for this send, or 0 if the request
// never got an HTTP response.
func (e *SendError) HTTPStatus() int {
	if e == nil {
		return 0
	}
	return e.status
}

// HTTPStatus returns the Resend HTTP status from err when err is (or wraps)
// a SendError. It returns 0 for nil, validation errors, network failures,
// and SDK errors that did not go through Send.
func HTTPStatus(err error) int {
	var se *SendError
	if errors.As(err, &se) {
		return se.HTTPStatus()
	}
	return 0
}

type httpStatusKey struct{}

// httpStatusSlot is request-scoped: meterTransport writes the response code
// so Send can wrap the SDK error without parsing it.
type httpStatusSlot struct {
	code       int
	retryAfter string
}
