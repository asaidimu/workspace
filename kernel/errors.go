// Package kernel turns every change into a command that runs authorized,
// ordered and atomic, and publishes events only after the commit succeeds.
//
// Errors: this package does not define an error type. Every error it returns
// is anansi's *common.SystemError (Workspace specification, sections 7 and
// 15), which carries a code, the failed operation, the target path, a
// severity, a retryable flag and structured per-field Issues, and unwraps to
// its cause so errors.Is and errors.As reach through every layer. Branch on a
// code with IsCode, or use errors.As for the full structure.
package kernel

import (
	"context"
	"errors"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/workspace/store"
)

// Error is anansi's SystemError. It is an alias, not a new type: a
// kernel.Error and a common.SystemError are the same value, so a host can
// receive one error from the kernel, from content or from an adapter and
// handle it with a single errors.As.
type Error = common.SystemError

// Code is SystemError's machine-readable code field. The kernel's error
// classes are the eight constants below; anything else in a code means the
// error came from deeper in the stack and was not reclassified.
type Code = string

const (
	CodeNotFound    Code = "NOT_FOUND"
	CodeConflict    Code = "CONFLICT"
	CodeInvalid     Code = "INVALID"
	CodeForbidden   Code = "FORBIDDEN"
	CodeBusy        Code = "BUSY"
	CodeCanceled    Code = "CANCELED"
	CodeUnavailable Code = "UNAVAILABLE"
	CodeInternal    Code = "INTERNAL"
)

// NewError builds an error in one of the kernel's classes. The returned value
// is a *SystemError, so the whole fluent builder is available:
//
//	kernel.NewError(kernel.CodeInvalid, "validation failed").
//	        WithOperation("conversation.create").
//	        WithIssue(issue)
//
// The builders return copies, so one base error can be specialised per
// subject without the subjects interfering.
func NewError(code Code, message string) *Error {
	return common.NewSystemError(code, message)
}

// WrapError builds an error with a cause. The cause stays on the chain, so
// errors.Is still reaches it — that is how a store sentinel survives being
// reclassified into a kernel code.
func WrapError(code Code, message string, cause error) *Error {
	return common.NewSystemError(code, message).WithCause(cause)
}

// IsCode reports whether err, or anything it wraps, carries the given code.
// It walks the chain itself rather than relying on SystemError.ContainsCode,
// because a cause that is not a SystemError (a store sentinel, a context
// error) must not stop the search, and because Issues attached at any depth
// also count. It is the sanctioned replacement for errors.Is, since the
// fluent builders return copies and no sentinel identity survives.
func IsCode(err error, code Code) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		se, ok := err.(*Error)
		if !ok {
			continue
		}
		if se.Code == code || se.Issues.ContainsCode(code) {
			return true
		}
	}
	return false
}

// CodeOf returns the code of the outermost classified error in the chain, or
// CodeInternal if the error carries no kernel code, or the empty string for a
// nil error. It never returns a code outside the eight classes, so a caller
// can switch on it unguarded.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	for ; err != nil; err = errors.Unwrap(err) {
		if se, ok := err.(*Error); ok && se.Code != "" {
			return se.Code
		}
	}
	return CodeInternal
}

// storeToCode maps the store package's sentinels to kernel codes, exactly as
// the table in Workspace specification section 7 requires: not-found to
// NOT_FOUND; conflict and already-exists to CONFLICT; invalid and too-large to
// INVALID; busy to BUSY; everything else to INTERNAL. Only classification
// lives here — no message — so one table serves every call site. A sentinel
// without its own class (ErrUnsupported, ErrTxClosed, ErrCorrupt) lands in
// INTERNAL but stays on the cause chain, so errors.Is still reaches it.
func storeToCode(err error) Code {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrAlreadyExists):
		return CodeConflict
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrTooLarge):
		return CodeInvalid
	case errors.Is(err, store.ErrBusy):
		return CodeBusy
	default:
		return CodeInternal
	}
}

// wrapStore converts a store error into a SystemError with the mapped code,
// keeping the store sentinel as the cause so errors.Is(err, store.ErrNotFound)
// still works. An error that already carries a code passes through untouched,
// so a nested classification is never overwritten. Only BUSY is flagged
// retryable: repeating an identical call is safe against transient contention,
// and unsafe against a conflict (the same expected revision fails again) or
// against an unsupported operation.
func wrapStore(err error) error {
	if err == nil {
		return nil
	}
	if se, ok := err.(*Error); ok && se.Code != "" {
		return err
	}
	code := storeToCode(err)
	e := common.NewSystemError(code, "store operation failed").WithCause(err)
	if code == CodeBusy {
		e = e.WithRetryable(true)
	}
	return e
}

// FromStore classifies a store sentinel as the section 7 code it maps to and
// returns it as a kernel error, keeping the sentinel as the cause so
// errors.Is(err, store.ErrNotFound) still reaches it. Packages outside the
// kernel — the agent loop, the facade — talk to repositories directly and
// would otherwise surface every backend failure as INTERNAL to a caller
// switching on IsCode, which is exactly the misclassification section 7's
// table exists to prevent. An error that already carries a code passes
// through untouched, so a nested classification is never overwritten.
func FromStore(err error) error {
	// A context error is CANCELED, not the INTERNAL a bare storeToCode
	// would give it; everything else is classified by sentinel.
	return wrapStore(wrapContext(err))
}

// wrapContext reclassifies a context error as CANCELED while keeping it on
// the chain, so a caller can tell "the caller gave up" from "the backend is
// down" without inspecting context.Canceled directly. Other errors pass
// through unchanged.
func wrapContext(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Error); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return common.NewSystemError(CodeCanceled, "operation canceled").WithCause(err)
	}
	return err
}
