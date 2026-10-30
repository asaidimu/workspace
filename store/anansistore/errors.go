package anansistore

import (
	"context"
	"errors"
	"strings"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/workspace/store"
)

// translate maps an anansi/persistence/sqlite error onto the port's sentinel
// errors. The mapping is pinned by the probe run recorded in
// todo/done/anansistore.md: duplicate unique-column inserts surface as
// ERR_PERSISTENCE_INSERT_DOCUMENTS_FAILED with a "UNIQUE constraint failed"
// message (or ERR_NATIVE_UNIQUE_CONSTRAINT_VIOLATION), schema violations as
// ERR_PERSISTENCE_VALIDATION_FAILED, and SQL busy as a retryable lock error.
// Anything else becomes ErrInternal.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrAlreadyExists),
		errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInvalid),
		errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrTxClosed),
		errors.Is(err, store.ErrUnsupported), errors.Is(err, store.ErrCorrupt),
		errors.Is(err, store.ErrTooLarge), errors.Is(err, store.ErrInternal):
		return err
	}

	var sysErr *common.SystemError
	code := ""
	msg := err.Error()
	if errors.As(err, &sysErr) {
		code = sysErr.Code
		if sysErr.Cause != nil {
			msg = sysErr.Cause.Error()
		}
		if sysErr.Message != "" {
			msg = sysErr.Message + " " + msg
		}
	}
	if c := causeMessage(err); c != "" {
		msg = c
	}
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(code, "VALIDATION_FAILED"):
		return store.ErrInvalid
	case strings.Contains(code, "UNIQUE_CONSTRAINT") || strings.Contains(lower, "unique constraint"):
		return store.ErrAlreadyExists
	case strings.Contains(lower, "database is locked") || strings.Contains(lower, "database table is locked") ||
		strings.Contains(code, "CANNOT_NEST_TRANSACTIONS"):
		return store.ErrBusy
	case strings.Contains(code, "COLLECTION_NOT_FOUND") || strings.Contains(lower, "no such collection") ||
		strings.Contains(lower, "collection '") && strings.Contains(lower, "does not exist"):
		return store.ErrNotFound
	}
	return store.ErrInternal
}

// causeMessage walks the error chain for the deepest system error message so
// translator decisions see the sqlite-level text (e.g. the UNIQUE constraint
// detail that lives on the cause, not the wrapper).
func causeMessage(err error) string {
	var sysErr *common.SystemError
	if errors.As(err, &sysErr) && sysErr.Cause != nil {
		return sysErr.Cause.Error()
	}
	return ""
}
