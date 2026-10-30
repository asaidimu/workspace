// Spike: Store specification section 15, items 6, 7, 10 on sqlite in-memory.
//
//  6. Performance of topic All queries through a junction collection, and
//     whether an in-memory intersection fallback is needed.
//  7. Keyset cursor stability on ID under inserts.
//  10. Whether the sqlite backend passes a contract slice (transactions,
//     hooks, read-your-writes, CAS, CRUD).
package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/asaidimu/go-anansi/v8"
	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/persistence/transaction"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	_ "github.com/mattn/go-sqlite3"
)

const linkSchemaJSON = `{
  "version": "1.0.0",
  "name": "Link",
  "fields": {
    "owner":  { "name": "owner",  "type": "string", "required": true },
    "kind":   { "name": "kind",   "type": "string", "required": true },
    "entity": { "name": "entity", "type": "string", "required": true },
    "topic":  { "name": "topic",  "type": "string", "required": true },
    "created":{ "name": "created","type": "string", "required": true }
  }
}`

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func cond(field string, op query.ComparisonOperator, val string) query.QueryFilter {
	v := val
	return query.QueryFilter{Condition: &query.FilterCondition{
		Field: field, Operator: op, Value: query.FilterValue{StringVal: &v},
	}}
}

// idCursorFilter is the keyset predicate: _id_ > lastID. Ordering on _id_
// alone is enough to page stably under inserts that land after the cursor.
func idCursorFilter(lastID string) query.QueryFilter {
	return cond(data.DocumentIDField, query.ComparisonOperatorGt, lastID)
}

func main() {
	ctx := context.Background()
	sch := must(definition.FromJSON([]byte(linkSchemaJSON)))
	p, cleanup, err := anansi.Playground(anansi.PlaygroundConfig{
		EnableLogging: false,
		EnableEvents:  false,
		Schemas:       []*definition.Schema{sch},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()
	links := must(p.Collection(ctx, "Link"))

	const n = 10000
	const topics = 25
	fmt.Println("S0 sqlite in-memory wired, seeding...")
	start := time.Now()
	batch := make([]data.Documenter, 0, 1000)
	for i := 0; i < n; i++ {
		d := must(data.NewDocument(map[string]any{
			"owner":   "o1",
			"kind":    "conversation",
			"entity":  fmt.Sprintf("e%04d", i),
			"topic":   fmt.Sprintf("t%d", i%topics),
			"created": fmt.Sprintf("2026-01-01T00:00:%06d", i),
		}))
		batch = append(batch, d)
		if len(batch) == 1000 {
			must2Create(many(links, batch))
			batch = batch[:0]
		}
	}
	must2Create(many(links, batch))

	// Multi-topic entities so All(t3,t7,t11) can match: e%04d for i<40 gets
	// the three extra topics so the in-memory intersection has real work.
	for i := 0; i < 40; i++ {
		extra := make([]data.Documenter, 0, 2)
		for _, t := range []string{"t7", "t11"} {
			extra = append(extra, must(data.NewDocument(map[string]any{
				"owner": "o1", "kind": "conversation",
				"entity": fmt.Sprintf("e%04d", i), "topic": t,
				"created": fmt.Sprintf("2026-01-01T00:00:%06d", 100000+i),
			})))
		}
		must2Create(many(links, extra))
	}
	fmt.Printf("S6 seed %d links in %v\n", n, time.Since(start))

	// --- Item 6: All = intersection over per-topic queries (fallback shape) ---
	want := []string{"t3", "t7", "t11"}
	start = time.Now()
	set := map[string]int{}
	for _, topic := range want {
		q := query.NewQueryBuilder().
			Where("owner").Eq("o1").
			Where("kind").Eq("conversation").
			Where("topic").Eq(topic).
			Limit(n).Build()
		res, err := links.Read(ctx, &q)
		if err != nil {
			log.Fatalf("topic %s: %v", topic, err)
		}
		for _, d := range res.Data {
			e, _ := d.GetString("entity")
			set[e]++
			d.Release()
		}
	}
	var matched []string
	for e, c := range set {
		if c == len(want) {
			matched = append(matched, e)
		}
	}
	sort.Strings(matched)
	fmt.Printf("S6 All(%v) via per-topic fallback: %v, in-memory intersection, matches=%d (want 40)\n",
		want, time.Since(start), len(matched))

	// single-topic baseline
	start = time.Now()
	q := query.NewQueryBuilder().Where("topic").Eq("t3").Limit(n).Build()
	res := must(links.Read(ctx, &q))
	fmt.Printf("S6 single-topic query on %d rows: %d rows in %v\n", n+80, res.Count, time.Since(start))
	for _, d := range res.Data {
		d.Release()
	}

	// --- Item 7: keyset cursor stability on ID under inserts ---
	seen := map[string]bool{}
	lastID := ""
	dupes, pages := 0, 0
	start = time.Now()
	for {
		if pages >= 250 {
			fmt.Println("S7 page cap reached (possible cursor stall)")
			break
		}
		qb := query.NewQueryBuilder().
			OrderBy(data.DocumentIDField, query.SortDirectionAsc).
			Limit(50)
		if lastID != "" {
			qb.AndFilter(idCursorFilter(lastID))
		}
		q := qb.Build()
		res, err := links.Read(ctx, &q)
		if err != nil {
			log.Fatalf("page %d: %v", pages, err)
		}
		if res.Count == 0 {
			for _, d := range res.Data {
				d.Release()
			}
			break
		}
		pages++
		for ri, d := range res.Data {
			id := d.ID()
			if pages <= 3 && (ri < 3 || ri == len(res.Data)-1) {
				fmt.Printf("    row[%d]: id=%s\n", ri, id)
			}
			if seen[id] {
				dupes++
			}
			seen[id] = true
			lastID = id
			d.Release()
			// interleave an insert: a pre-cursor row must not disturb paging
			if pages == 2 && ri == 0 {
				nd := must(data.NewDocument(map[string]any{
					"owner": "o1", "kind": "conversation",
					"entity": "injected", "topic": "t3",
					"created": "2025-12-31T23:59:59.000000",
				}))
				if _, err := links.CreateOne(ctx, nd); err != nil {
					log.Fatal(err)
				}
			}
		}
		if res.Count < 50 {
			break
		}
	}
	fmt.Printf("S7 keyset pages=%d distinct=%d dupes=%d elapsed=%v (want dupes=0)\n",
		pages, len(seen), dupes, time.Since(start))

	// --- Item 10: contract slice on sqlite in-memory ---
	var hooks []string
	_, err = links.Transact(ctx, func(tctx context.Context) (any, error) {
		tx, ok := transaction.GetCurrentTransaction(tctx)
		if !ok {
			return nil, fmt.Errorf("no transaction in tctx")
		}
		tx.OnCommit(func() { hooks = append(hooks, "commit") })
		tx.OnRollback(func() { hooks = append(hooks, "rollback") })
		d := must(data.NewDocument(map[string]any{
			"owner": "o2", "kind": "c", "entity": "x1", "topic": "t", "created": "2026-02-01T00:00:00.000000",
		}))
		if _, err := links.CreateOne(tctx, d); err != nil {
			return nil, err
		}
		q := query.NewQueryBuilder().Where("entity").Eq("x1").Build()
		rr, err := links.Read(tctx, &q)
		if err != nil {
			return nil, err
		}
		fmt.Printf("S10 sqlite read-your-writes: %d (want 1)\n", rr.Count)
		return nil, nil
	})
	fmt.Printf("S10 sqlite commit hooks=%v err=%v\n", hooks, err)

	hooks = nil
	_, err = links.Transact(ctx, func(tctx context.Context) (any, error) {
		tx, _ := transaction.GetCurrentTransaction(tctx)
		tx.OnCommit(func() { hooks = append(hooks, "commit") })
		tx.OnRollback(func() { hooks = append(hooks, "rollback") })
		d := must(data.NewDocument(map[string]any{
			"owner": "o3", "kind": "c", "entity": "x2", "topic": "t", "created": "2026-02-02T00:00:00.000000",
		}))
		if _, err := links.CreateOne(tctx, d); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("rollback")
	})
	q2 := query.NewQueryBuilder().Where("entity").Eq("x2").Build()
	after := must(links.Read(ctx, &q2))
	fmt.Printf("S10 sqlite rollback hooks=%v rolled-back-write-visible=%d (want 0) err=%v\n", hooks, after.Count, err != nil)

	// CAS on sqlite: version mismatch -> 0 rows?
	created := must(links.CreateOne(ctx, must(data.NewDocument(map[string]any{
		"owner": "o4", "kind": "c", "entity": "x3", "topic": "t", "created": "2026-02-03T00:00:00.000000",
	}))))
	cid := created.Data.ID()
	cv := must(created.Data.Version())
	idF := query.NewQueryBuilder().Where(data.DocumentIDField).Eq(cid).Build().Filters
	stale := cv - 1
	uq, err := links.Update(ctx, &base.CollectionUpdate{
		Set:     must(data.NewDocument(map[string]any{"topic": "changed"})),
		Filter:  idF,
		Version: &stale,
	})
	fmt.Printf("S10 sqlite CAS stale: count=%d err=%v (want count=0)\n", cnt(uq), err)
}

func cnt(r *base.ReadResult) int {
	if r == nil {
		return -1
	}
	return r.Count
}

func many(c base.Collection, docs []data.Documenter) ([]base.CreateResult, error) {
	return c.CreateMany(context.Background(), docs)
}

func must2Create(_ []base.CreateResult, err error) {
	if err != nil {
		log.Fatal(err)
	}
}
