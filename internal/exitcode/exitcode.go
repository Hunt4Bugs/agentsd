// Package exitcode defines the stable process exit codes (spec §11) and the
// API error codes that map onto them (spec §12).
package exitcode

import (
	"errors"
	"fmt"
)

const (
	OK                 = 0
	Error              = 1
	Usage              = 2
	DaemonUnreachable  = 3
	NotFound           = 4
	Invalid            = 5
	ChangesPending     = 6
	RuntimeUnavailable = 7
	Concurrency        = 8
	RunFailed          = 10
	RunStopped         = 11
	RunTimedOut        = 12
)

// API error codes, as used in {"error": {"code": ...}}.
const (
	CodeInternal           = "internal"
	CodeUsage              = "bad_request"
	CodeDaemonUnreachable  = "daemon_unreachable"
	CodeNotFound           = "not_found"
	CodeInvalid            = "invalid"
	CodePolicy             = "policy"
	CodeChangesPending     = "changes_pending"
	CodeRuntimeUnavailable = "runtime_unavailable"
	CodeConcurrency        = "concurrency_limit"
	CodeRunFailed          = "run_failed"
	CodeRunStopped         = "run_stopped"
	CodeRunTimedOut        = "run_timed_out"
)

var codeToExit = map[string]int{
	CodeInternal:           Error,
	CodeUsage:              Usage,
	CodeDaemonUnreachable:  DaemonUnreachable,
	CodeNotFound:           NotFound,
	CodeInvalid:            Invalid,
	CodePolicy:             Invalid,
	CodeChangesPending:     ChangesPending,
	CodeRuntimeUnavailable: RuntimeUnavailable,
	CodeConcurrency:        Concurrency,
	CodeRunFailed:          RunFailed,
	CodeRunStopped:         RunStopped,
	CodeRunTimedOut:        RunTimedOut,
}

// FromCode maps an API error code to an exit code. Unknown codes map to 1.
func FromCode(code string) int {
	if c, ok := codeToExit[code]; ok {
		return c
	}
	return Error
}

// E is an error that carries an API code (and therefore an exit code).
type E struct {
	Code string
	Msg  string
	Err  error
}

func (e *E) Error() string {
	if e.Err != nil && e.Msg != "" {
		return e.Msg + ": " + e.Err.Error()
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Msg
}

func (e *E) Unwrap() error { return e.Err }

// New returns an *E with a formatted message.
func New(code, format string, args ...any) *E {
	return &E{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap returns an *E wrapping err.
func Wrap(code string, err error, msg string) *E {
	return &E{Code: code, Msg: msg, Err: err}
}

// Of returns the exit code for err: 0 for nil, the mapped code for *E, and 1
// otherwise.
func Of(err error) int {
	if err == nil {
		return OK
	}
	var e *E
	if errors.As(err, &e) {
		return FromCode(e.Code)
	}
	return Error
}

// CodeOf returns the API code for err (internal when it carries none).
func CodeOf(err error) string {
	var e *E
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// Silent wraps an exit code whose message has already been printed.
type Silent struct{ Code int }

func (s *Silent) Error() string { return fmt.Sprintf("exit %d", s.Code) }
