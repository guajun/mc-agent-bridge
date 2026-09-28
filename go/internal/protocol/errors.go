package protocol

import (
	"encoding/json"
	"fmt"
)

// Stable error codes. Harnesses branch on these, never on message text.
const (
	CodeUsage                  = "usage"
	CodeTargetRequired         = "target_required"
	CodeTargetUnknown          = "target_unknown"
	CodeDaemonNotRunning       = "daemon_not_running"
	CodeDaemonAlreadyRunning   = "daemon_already_running"
	CodeUnauthorized           = "unauthorized"
	CodeConnectionFailed       = "connection_failed"
	CodeConnectionLost         = "connection_lost"
	CodeTimeout                = "timeout"
	CodeCapabilityNotSupported = "capability_not_supported"
	CodeForbidden              = "forbidden"
	CodeGameError              = "game_error"
	CodeBadRequest             = "bad_request"
	CodeResultUnknown          = "result_unknown"
	CodeInternal               = "internal"
	CodeUnsupportedTransport   = "unsupported_transport"
	CodeNotFound               = "not_found"
)

// Error is the stable machine-readable error shape on stdout, over the local
// IPC and inside the remote protocol.
type Error struct {
	Code          string          `json:"code"`
	Message       string          `json:"message"`
	Retryable     bool            `json:"retryable"`
	ResultUnknown bool            `json:"resultUnknown"`
	RequestID     string          `json:"requestId,omitempty"`
	Operation     string          `json:"operation,omitempty"`
	Target        string          `json:"target,omitempty"`
	Hint          string          `json:"hint,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds a plain error value.
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// ExitCode maps an error code onto the documented process exit code. The CLI
// contract keeps these stable for scripts.
func ExitCode(code string) int {
	switch code {
	case "":
		return 0
	case CodeUsage:
		return 2
	case CodeTargetRequired, CodeTargetUnknown, CodeNotFound:
		return 3
	case CodeConnectionFailed, CodeConnectionLost, CodeDaemonNotRunning:
		return 4
	case CodeUnauthorized, CodeForbidden:
		return 5
	case CodeCapabilityNotSupported, CodeUnsupportedTransport:
		return 6
	case CodeTimeout, CodeResultUnknown:
		return 7
	default:
		return 1
	}
}

// ErrorFrom parses an error object out of a remote reply.
func ErrorFrom(code, message string, retryable, resultUnknown bool, requestID, operation string) *Error {
	return &Error{
		Code:          code,
		Message:       message,
		Retryable:     retryable,
		ResultUnknown: resultUnknown,
		RequestID:     requestID,
		Operation:     operation,
	}
}
