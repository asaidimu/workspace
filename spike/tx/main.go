// Spike: Store specification section 15, items 1, 2, 5.
//
//  1. How OnCommit/OnRollback are reached from inside Persistence.Transact.
//  2. How a version mismatch on update is reported and whether it maps to ErrConflict.
//  5. Snapshot semantics and read-your-writes inside Transact.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/asaidimu/go-anansi/v8"
	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/persistence/transaction"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	_ "github.com/mattn/go-sqlite3"
)

const acctSchemaJSON = `{
  "version": "1.0.0",
  "name": "Acct",
  "fields": {
    "name":    { "name": "name",    "type": "string",  "required": true },
    "balance": { "name": "balance", "type": "integer", "required": true }
  }
}`

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func main() {
	ctx := context.Background()
	sch := must(definition.FromJSON([]byte(acctSchemaJSON)))
	p, cleanup, err := anansi.Playground(anansi.PlaygroundConfig{
		EnableLogging: false,
		EnableEvents:  false,
		Schemas:       []*definition.Schema{sch},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	acct := must(p.Collection(ctx, "Acct"))
	newDoc := func(name string, bal int) data.Documenter {
		d, err := data.NewDocument(map[string]any{"name": name, "balance": bal})
		if err != nil {
			log.Fatal(err)
		}
		return d
	}

	// --- Item 1: hooks from inside a transaction, commit path ---
	var hooks []string
	_, err = acct.Transact(ctx, func(tctx context.Context) (any, error) {
		tx, ok := transaction.GetCurrentTransaction(tctx)
		if !ok {
			return nil, errors.New("no current transaction in tctx")
		}
		tx.OnCommit(func() { hooks = append(hooks, "commit") })
		tx.OnRollback(func() { hooks = append(hooks, "rollback") })

		if _, err := acct.CreateOne(tctx, newDoc("a", 100)); err != nil {
			return nil, err
		}

		// --- Item 5: read-your-writes inside the tx ---
		q := query.NewQueryBuilder().From("Acct").Where("name").Eq("a").Build()
		rr, err := acct.Read(tctx, &q)
		if err != nil {
			return nil, err
		}
		fmt.Printf("S5 read-your-writes inside tx: count=%d\n", rr.Count)
		return nil, nil // commit
	})
	fmt.Printf("S1 commit path: err=%v hooks=%v\n", err, hooks)

	// rollback path
	hooks = nil
	_, err = acct.Transact(ctx, func(tctx context.Context) (any, error) {
		tx, _ := transaction.GetCurrentTransaction(tctx)
		tx.OnCommit(func() { hooks = append(hooks, "commit") })
		tx.OnRollback(func() { hooks = append(hooks, "rollback") })
		return nil, errors.New("force rollback")
	})
	fmt.Printf("S1 rollback path: err=%v hooks=%v\n", err != nil, hooks)

	// --- Item 2: version mismatch ---
	created := must(acct.CreateOne(ctx, newDoc("b", 50)))
	id := created.Data.ID()
	q := query.NewQueryBuilder().From("Acct").Where(data.DocumentIDField).Eq(id).Limit(1).Build()
	got := must(acct.Read(ctx, &q))
	if len(got.Data) == 0 {
		log.Fatal("just-created doc not found")
	}
	meta := must(got.Data[0].Version())
	fmt.Printf("S2 doc id=%s version=%d\n", id, meta)
	got.Data[0].Release()

	idFilter := query.NewQueryBuilder().Where(data.DocumentIDField).Eq(id).Build().Filters
	rd := true
	stale := meta - 1
	res, err := acct.Update(ctx, &base.CollectionUpdate{
		Set:            must(data.NewDocument(map[string]any{"balance": 999})),
		Filter:         idFilter,
		Version:        &stale,
		ReturnDocument: &rd,
	})
	fmt.Printf("S2 stale version=%d: count=%d rows=%d err=%v\n", stale, count0(res), rows(res), err)

	// control: correct version
	res2, err2 := acct.Update(ctx, &base.CollectionUpdate{
		Set:            must(data.NewDocument(map[string]any{"balance": 1000})),
		Filter:         idFilter,
		Version:        &meta,
		ReturnDocument: &rd,
	})
	fmt.Printf("S2 correct version=%d: count=%d rows=%d err=%v\n", meta, count0(res2), rows(res2), err2)

	// read back to see what actually happened
	finalQ := query.NewQueryBuilder().From("Acct").Where(data.DocumentIDField).Eq(id).Limit(1).Build()
	final := must(acct.Read(ctx, &finalQ))
	if len(final.Data) > 0 {
		bal, _ := final.Data[0].GetInt("balance")
		ver, _ := final.Data[0].Version()
		fmt.Printf("S2 after both updates: balance=%d version=%d\n", bal, ver)
		final.Data[0].Release()
	} else {
		fmt.Println("S2 after both updates: doc missing")
	}

	// --- Item 5: nested Transact joins the outer transaction ---
	var sameTx bool
	_, err = acct.Transact(ctx, func(tctx context.Context) (any, error) {
		outer, _ := transaction.GetCurrentTransaction(tctx)
		_, err := acct.Transact(tctx, func(tctx2 context.Context) (any, error) {
			inner, ok := transaction.GetCurrentTransaction(tctx2)
			sameTx = ok && inner == outer
			if _, err := acct.CreateOne(tctx2, newDoc("c", 1)); err != nil {
				return nil, err
			}
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
		return nil, errors.New("roll back the whole nested work")
	})
	fmt.Printf("S5 nested join: same-tx=%v outer-rolled-back err=%v\n", sameTx, err != nil)

	// verify the nested create was rolled back
	afterQ := query.NewQueryBuilder().From("Acct").Where("name").Eq("c").Build()
	after := must(acct.Read(ctx, &afterQ))
	fmt.Printf("S5 nested create visible after rollback: %d (want 0)\n", after.Count)
}

func count0(r *base.ReadResult) int {
	if r == nil {
		return -1
	}
	return r.Count
}

func rows(r *base.ReadResult) int {
	if r == nil {
		return -1
	}
	return len(r.Data)
}
