package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/workspace/store"
)

// TestStoreSentinelCodes pins the translation table from Workspace
// specification section 7. If a mapping changes, this test must fail: callers
// branch on the code, so the table is a contract, not an implementation
// detail.
func TestStoreSentinelCodes(t *testing.T) {
	cases := []struct {
		sentinel error
		want     Code
	}{
		{store.ErrNotFound, CodeNotFound},
		{store.ErrConflict, CodeConflict},
		{store.ErrAlreadyExists, CodeConflict},
		{store.ErrInvalid, CodeInvalid},
		{store.ErrTooLarge, CodeInvalid},
		{store.ErrBusy, CodeBusy},
		// Everything else is INTERNAL, per the table.
		{store.ErrUnsupported, CodeInternal},
		{store.ErrTxClosed, CodeInternal},
		{store.ErrCorrupt, CodeInternal},
		{store.ErrInternal, CodeInternal},
		{errors.New("something else entirely"), CodeInternal},
	}
	for _, tc := range cases {
		if got := storeToCode(tc.sentinel); got != tc.want {
			t.Errorf("storeToCode(%v) = %q, want %q", tc.sentinel, got, tc.want)
		}
	}
}

// TestWrapStoreKeepsSentinel checks both halves of the contract at once: the
// error carries the kernel code, and the store sentinel is still reachable
// through errors.Is so a caller can branch either way.
func TestWrapStoreKeepsSentinel(t *testing.T) {
	wrapped := wrapStore(store.ErrNotFound)

	if !IsCode(wrapped, CodeNotFound) {
		t.Errorf("IsCode(wrapped, NOT_FOUND) = false, want true; err = %v", wrapped)
	}
	if !errors.Is(wrapped, store.ErrNotFound) {
		t.Errorf("errors.Is(wrapped, store.ErrNotFound) = false, want true")
	}

	var se *Error
	if !errors.As(wrapped, &se) {
		t.Fatalf("errors.As to *kernel.Error = false, want true")
	}
	if se.Code != CodeNotFound {
		t.Errorf("se.Code = %q, want %q", se.Code, CodeNotFound)
	}
	if se.Cause != store.ErrNotFound {
		t.Errorf("se.Cause = %v, want the store sentinel", se.Cause)
	}
}

// TestWrapStoreRetryable pins that only BUSY is auto-retryable. A conflict
// replays the same stale revision and fails again, and an unsupported
// operation never becomes supported, so flagging either as retryable would
// send a caller into a loop.
func TestWrapStoreRetryable(t *testing.T) {
	if !wrapStore(store.ErrBusy).(*Error).Retryable {
		t.Error("BUSY should be retryable")
	}
	for _, err := range []error{
		store.ErrConflict, store.ErrAlreadyExists, store.ErrNotFound,
		store.ErrInvalid, store.ErrUnsupported, store.ErrCorrupt,
	} {
		if wrapStore(err).(*Error).Retryable {
			t.Errorf("%v should not be retryable", err)
		}
	}
}

// TestWrapStoreDoesNotReclassify proves a nested error that already carries a
// code keeps it, so a classification made deeper in the stack survives the
// trip up through the kernel.
func TestWrapStoreDoesNotReclassify(t *testing.T) {
	inner := common.NewSystemError(CodeForbidden, "denied by policy")
	if got := wrapStore(inner); got != error(inner) {
		t.Errorf("wrapStore reclassified an already-coded error: got %v, want the original", got)
	}
}

// TestWrapStoreNil makes the nil path a no-op, so a caller can wrap an
// unchecked error return.
func TestWrapStoreNil(t *testing.T) {
	if got := wrapStore(nil); got != nil {
		t.Errorf("wrapStore(nil) = %v, want nil", got)
	}
	if got := wrapContext(nil); got != nil {
		t.Errorf("wrapContext(nil) = %v, want nil", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q, want empty", got)
	}
}

// TestWrapContext checks that a caller giving up is distinguishable from a
// backend failing, while the original context error stays reachable.
func TestWrapContext(t *testing.T) {
	wrapped := wrapContext(context.Canceled)
	if !IsCode(wrapped, CodeCanceled) {
		t.Errorf("wrapContext(context.Canceled) code = %q, want CANCELED", CodeOf(wrapped))
	}
	if !errors.Is(wrapped, context.Canceled) {
		t.Error("errors.Is(wrapped, context.Canceled) = false, want true")
	}

	if got := wrapContext(store.ErrNotFound); IsCode(got, CodeCanceled) {
		t.Error("a store error must not be reclassified as CANCELED")
	}
}

// TestIsCodeWalksChain proves IsCode finds a code anywhere in the chain,
// including through a non-SystemError cause such as a store sentinel, which
// would stop a naive single-level check.
func TestIsCodeWalksChain(t *testing.T) {
	outer := NewError(CodeInternal, "outer").WithCause(wrapStore(store.ErrConflict))
	if !IsCode(outer, CodeConflict) {
		t.Error("IsCode did not find the inner CONFLICT through a non-coded wrapper")
	}
	if !IsCode(outer, CodeInternal) {
		t.Error("IsCode did not find the outer INTERNAL")
	}
	if IsCode(outer, CodeNotFound) {
		t.Error("IsCode reported a code that is not present")
	}
}

// TestIsCodeFindsIssues covers the per-field detail the spec requires: a
// validation issue's code is matchable without parsing the message.
func TestIsCodeFindsIssues(t *testing.T) {
	err := NewError(CodeInvalid, "validation failed").WithIssue(common.Issue{
		Code:    "REQUIRED_FIELD_MISSING",
		Message: "title is required",
		Path:    "title",
	})
	if !IsCode(err, CodeInvalid) {
		t.Error("IsCode did not match the error's own code")
	}
	if !IsCode(err, "REQUIRED_FIELD_MISSING") {
		t.Error("IsCode did not match the issue code")
	}
	if !IsCode(NewError(CodeInvalid, "wrapped").WithCause(err), "REQUIRED_FIELD_MISSING") {
		t.Error("IsCode did not match an issue code through a wrapping cause")
	}
}

// TestCodeOf checks the outermost-code rule and the INTERNAL fallback for an
// error with no kernel code.
func TestCodeOf(t *testing.T) {
	if got := CodeOf(NewError(CodeConflict, "x")); got != CodeConflict {
		t.Errorf("CodeOf = %q, want CONFLICT", got)
	}
	if got := CodeOf(errors.New("plain")); got != CodeInternal {
		t.Errorf("CodeOf(plain error) = %q, want INTERNAL", got)
	}
	// An outer error with no code of its own must not hide the code below it.
	if got := CodeOf(NewError("", "no code").WithCause(store.ErrNotFound)); got != CodeInternal {
		t.Errorf("CodeOf = %q, want INTERNAL (the outer error carries no code)", got)
	}
}

// TestWrapError checks the cause lands on the chain and stays reachable.
func TestWrapError(t *testing.T) {
	cause := store.ErrConflict
	wrapped := WrapError(CodeConflict, "write lost the race", cause)

	if wrapped.Code != CodeConflict {
		t.Errorf("Code = %q, want CONFLICT", wrapped.Code)
	}
	if wrapped.Message != "write lost the race" {
		t.Errorf("Message = %q", wrapped.Message)
	}
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is through the cause = false, want true")
	}
}

// TestWrapContextLeavesClassifiedAlone proves an error that already carries a
// kernel code is not relabelled CANCELED on the way out, even when its cause
// happens to be a context error.
func TestWrapContextLeavesClassifiedAlone(t *testing.T) {
	classified := NewError(CodeUnavailable, "no provider").WithCause(context.Canceled)
	if got := wrapContext(classified); got != error(classified) {
		t.Errorf("wrapContext reclassified an already-coded error: got %v", got)
	}
}

// TestErrorIsSystemError proves the alias is an alias: one errors.As matches
// an error from this package and from anansi's, so a host needs one check.
func TestErrorIsSystemError(t *testing.T) {
	var se *common.SystemError
	if !errors.As(NewError(CodeNotFound, "x"), &se) {
		t.Fatal("errors.As to *common.SystemError = false, want true")
	}
	var ke *Error
	if !errors.As(common.NewSystemError(CodeNotFound, "x"), &ke) {
		t.Fatal("kernel.Error did not match an anansi SystemError")
	}
}
