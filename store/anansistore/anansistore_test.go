package anansistore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/persistence/persistence"
	"github.com/asaidimu/go-anansi/v8/core/query/native"
	sqliteExecutor "github.com/asaidimu/go-anansi/v8/sqlite/executor"
	sqliteQuery "github.com/asaidimu/go-anansi/v8/sqlite/query"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/storetest"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// fixedTime matches storetest's clock so timestamp assertions are exact.
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

// newTestPersistence wires an ephemeral SQLite-backed persistence, the same
// wiring the spike verified: sqlite executor + factory → native interactor →
// persistence. One in-memory DB per factory call with a single connection,
// so the whole suite serializes on one writer exactly like the spec's
// single-writer SQLite deployment.
func newTestPersistence(t testing.TB) base.Persistence {
	logger := zap.NewNop()
	if err := data.ConfigureDocumentFactory(data.DocumentFactoryConfig{}, logger); err != nil {
		t.Fatalf("configure document factory: %v", err)
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	exec, err := sqliteExecutor.NewSQLiteExecutor(db, logger)
	if err != nil {
		t.Fatalf("sqlite executor: %v", err)
	}
	ix, err := native.NewNativeInteractor(exec, sqliteQuery.NewSQLiteFactory(logger), logger)
	if err != nil {
		t.Fatalf("native interactor: %v", err)
	}
	p, err := persistence.NewPersistence(ix, nil, logger, nil)
	if err != nil {
		t.Fatalf("persistence: %v", err)
	}
	return p
}

// TestContract runs the shared store conformance suite against the anansi
// adapter. If memory and anansistore both pass, the kernel cannot tell them
// apart at the store boundary.
func TestContract(t *testing.T) {
	storetest.Run(t, func(t testing.TB) store.Store {
		p := newTestPersistence(t)
		s, err := New(p, Options{Clock: clock{t: fixedTime}, Logger: zap.NewNop()})
		if err != nil {
			t.Fatalf("anansistore.New: %v", err)
		}
		// sanity: the collections exist after construction
		ctx := context.Background()
		for _, spec := range collectionSpecs {
			name := "ws_" + spec.suffix
			has, err := p.HasCollection(ctx, name)
			if err != nil || !has {
				t.Fatalf("collection %q missing: has=%v err=%v", name, has, err)
			}
		}
		return s
	})
}
