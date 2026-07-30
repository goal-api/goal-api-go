package goalapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Error is the only error type this SDK returns. Branch on the class of failure with
// errors.Is against the sentinels below, and get at the fields with errors.As:
//
//	var apiErr *goalapi.Error
//	if errors.As(err, &apiErr) {
//	    log.Printf("%s (correlation %s)", apiErr.Code, apiErr.CorrelationID)
//	}
type Error struct {
	// StatusCode is the HTTP status, or 0 for a network/timeout failure.
	StatusCode int
	// Message comes from the response body, or from the SDK on a transport failure.
	Message string
	// Code is the API's machine-readable code, e.g. "VALIDATION_ERROR".
	Code string
	// Category groups the code, e.g. "validation", "not_found".
	Category string
	// Details is per-field validation info: an object from the gateway, an array from
	// football-service.
	Details json.RawMessage
	// CorrelationID is set on gateway errors only, not on football-service ones.
	CorrelationID string

	// Timeout is true when the request exceeded the client timeout.
	Timeout bool
	// Network is true when no HTTP response was produced at all.
	Network bool

	// RetryAfter is the server-requested wait in seconds, set on 429.
	RetryAfter int
	// Limit, Remaining, Reset and RateLimitType mirror the X-RateLimit-* headers on a 429.
	Limit         int
	Remaining     int
	Reset         int64
	RateLimitType string

	wrapped error
}

func (e *Error) Error() string {
	parts := make([]string, 0, 4)
	parts = append(parts, "goalapi: "+e.Message)
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.CorrelationID != "" {
		parts = append(parts, "correlation_id="+e.CorrelationID)
	}
	return strings.Join(parts, " ")
}

func (e *Error) Unwrap() error { return e.wrapped }

// Is maps the sentinels below onto status codes.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrValidation:
		return e.StatusCode == http.StatusBadRequest || e.StatusCode == http.StatusUnprocessableEntity
	case ErrAuthentication:
		return e.StatusCode == http.StatusUnauthorized
	case ErrPermission:
		return e.StatusCode == http.StatusForbidden
	case ErrPlanUpgradeRequired:
		return e.StatusCode == http.StatusPaymentRequired
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrServiceUnavailable:
		return e.StatusCode == http.StatusServiceUnavailable
	case ErrServer:
		return e.StatusCode >= 500
	case ErrTimeout:
		return e.Timeout
	case ErrNetwork:
		return e.Network
	}
	return false
}

// Sentinels for errors.Is. See Error.Is for the mapping.
var (
	ErrValidation          = sentinel("validation failed")
	ErrAuthentication      = sentinel("authentication failed")
	ErrPermission          = sentinel("access denied")
	ErrPlanUpgradeRequired = sentinel("plan upgrade required")
	ErrNotFound            = sentinel("not found")
	ErrConflict            = sentinel("conflict")
	ErrRateLimited         = sentinel("rate limited")
	ErrServiceUnavailable  = sentinel("service unavailable")
	ErrServer              = sentinel("server error")
	ErrTimeout             = sentinel("request timed out")
	ErrNetwork             = sentinel("network error")
)

type sentinelError string

func sentinel(s string) error         { return sentinelError(s) }
func (s sentinelError) Error() string { return "goalapi: " + string(s) }

// errorEnvelope covers both error shapes: the gateway sends Message, football-service
// sends Error.
type errorEnvelope struct {
	Message       string          `json:"message"`
	Error         string          `json:"error"`
	Code          string          `json:"code"`
	Category      string          `json:"category"`
	Details       json.RawMessage `json:"details"`
	CorrelationID string          `json:"correlationId"`
}

func errorFromResponse(status int, body []byte, headers http.Header) *Error {
	err := &Error{StatusCode: status}

	var envelope errorEnvelope
	if jsonErr := json.Unmarshal(body, &envelope); jsonErr == nil {
		err.Message = envelope.Message
		if err.Message == "" {
			err.Message = envelope.Error
		}
		err.Code = envelope.Code
		err.Category = envelope.Category
		err.Details = envelope.Details
		err.CorrelationID = envelope.CorrelationID
	}

	if err.Message == "" {
		// nginx returns HTML for 502s. Keep an excerpt so the message is still useful.
		if excerpt := strings.TrimSpace(string(body)); excerpt != "" && !strings.HasPrefix(excerpt, "{") {
			if len(excerpt) > 200 {
				excerpt = excerpt[:200]
			}
			err.Message = excerpt
		} else {
			err.Message = fmt.Sprintf("HTTP %d", status)
		}
	}

	if status == http.StatusTooManyRequests {
		err.RetryAfter = atoiOrZero(headers.Get("Retry-After"))
		err.Limit = atoiOrZero(headers.Get("X-RateLimit-Limit"))
		err.Remaining = atoiOrZero(headers.Get("X-RateLimit-Remaining"))
		err.Reset = int64(atoiOrZero(headers.Get("X-RateLimit-Reset")))
		err.RateLimitType = headers.Get("X-RateLimit-Type")
	}

	return err
}
