// Spike: Store specification section 15, items 3 and 9.
//
//  3. Raw template parameter binding, the recursive query's cost, and how the
//     adapter detects recursive-query support.
//  9. The overhead of the pooled-document release discipline (raw Read +
//     Release vs model binding).
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/asaidimu/go-anansi/v8"
	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/document"
	"github.com/asaidimu/go-anansi/v8/core/persistence/collection"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

const nodeSchemaJSON = `{
  "version": "1.0.0",
  "name": "Node",
  "fields": {
    "name":   { "name": "name",   "type": "string", "required": true },
    "parent": { "name": "parent", "type": "string", "required": false }
  }
}`

type Node struct {
	document.DocumentModel
	Name   string `anansi:"name"`
	Parent string `anansi:"parent"`
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func main() {
	ctx := context.Background()
	sch := must(definition.FromJSON([]byte(nodeSchemaJSON)))
	p, cleanup, err := anansi.Playground(anansi.PlaygroundConfig{
		EnableLogging: false,
		EnableEvents:  false,
		Schemas:       []*definition.Schema{sch},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	nodes := must(p.Collection(ctx, "Node"))

	// seed a chain n0 <- n1 <- ... <- n19 (parent = child's parent name)
	for i := 0; i < 20; i++ {
		d := must(data.NewDocument(map[string]any{"name": fmt.Sprintf("n%d", i)}))
		if i > 0 {
			must2(d.Set("parent", fmt.Sprintf("n%d", i-1)))
		}
		if _, err := nodes.CreateOne(ctx, d); err != nil {
			log.Fatalf("seed %d: %v", i, err)
		}
	}

	// debug: what tables does the raw-query connection see?
	qr, err := p.Query(ctx, &query.RawQuery{Template: `SELECT name FROM sqlite_master WHERE type='table'`})
	fmt.Printf("S3 debug tables: err=%v data=%v\n", err, qrData(qr))

	// --- Item 3a: bound parameters ---
	targets := map[string]query.RawQueryTarget{"n": {Collection: "Node"}}
	qr, err = p.Query(ctx, &query.RawQuery{
		Template:    `SELECT _id_, name FROM {{collections.n}} WHERE name = ?`,
		Collections: targets,
		Parameters:  []any{"n5"},
	})
	fmt.Printf("S3 simple bind: err=%v count=%d data=%v\n", err, count(qr), qrData(qr))

	// --- Item 3b: injection attempt stays data ---
	qr, err = p.Query(ctx, &query.RawQuery{
		Template:    `SELECT _id_, name FROM {{collections.n}} WHERE name = ?`,
		Collections: targets,
		Parameters:  []any{"x'; DROP TABLE Node;--"},
	})
	fmt.Printf("S3 injection: err=%v count=%d (table must survive)\n", err, count(qr))
	after, err := p.Query(ctx, &query.RawQuery{Template: `SELECT COUNT(*) AS c FROM {{collections.n}}`, Collections: targets})
	fmt.Printf("S3 table after injection attempt: err=%v count=%d (want count=1 row)\n", err, count(after))

	qr, err = p.Query(ctx, &query.RawQuery{Template: `SELECT name, parent FROM {{collections.n}} WHERE parent IS NOT NULL`, Collections: targets})
	fmt.Printf("S3 debug parents: err=%v count=%d\n", err, count(qr))
	qr, err = p.Query(ctx, &query.RawQuery{Template: `WITH RECURSIVE w(name) AS (SELECT name FROM {{collections.n}} WHERE name='n0') SELECT name FROM w`, Collections: targets})
	fmt.Printf("S3 debug anchor-only CTE: err=%v count=%d\n", err, count(qr))

	// --- Item 3c: WITH RECURSIVE + capability probe ---
	recursiveTemplate := `
WITH RECURSIVE walk(id, name, depth, path) AS (
  SELECT _id_, name, 0, name FROM {{collections.n}} WHERE name = 'n0'
  UNION ALL
  SELECT c._id_, c.name, w.depth + 1, w.path || '/' || c.name
  FROM {{collections.n}} c JOIN walk w ON c.parent = w.name
)
SELECT name, depth FROM walk ORDER BY depth`
	start := time.Now()
	qr, err = p.Query(ctx, &query.RawQuery{Template: recursiveTemplate, Collections: targets})
	elapsed := time.Since(start)
	fmt.Printf("S3 WITH RECURSIVE bare: err=%v count=%d (0 = upstream executor bug: only SELECT-prefixed SQL returns rows)\n", err, count(qr))

	// workaround: wrap so the statement starts with SELECT
	wrapped := `SELECT * FROM (` + recursiveTemplate + `)`
	qr2, err2 := p.Query(ctx, &query.RawQuery{Template: wrapped, Collections: targets})
	fmt.Printf("S3 WITH RECURSIVE wrapped in SELECT(...): err=%v count=%d dataLen=%d\n", err2, count(qr2), dataLen(qr2))
	_ = qr2
	fmt.Printf("S3 WITH RECURSIVE: err=%v count=%d dataLen=%d depth20-walk=%v\n", err, count(qr), dataLen(qr), elapsed)
	if err != nil {
		fmt.Printf("S3 capability probe result: RecursiveQueries=false (%v)\n", err)
	} else {
		fmt.Printf("S3 capability probe result: RecursiveQueries=true (probe query succeeded)\n")
	}

	// chain cost: walk from deepest leaf to root repeatedly
	deep := `
WITH RECURSIVE walk(id, name, depth) AS (
  SELECT _id_, name, 0 FROM {{collections.n}} WHERE name = 'n19'
  UNION ALL
  SELECT p._id_, p.name, w.depth + 1 FROM {{collections.n}} p JOIN walk w ON w.name = p.parent
)
SELECT depth FROM walk ORDER BY depth`
	const rounds = 50
	start = time.Now()
	for i := 0; i < rounds; i++ {
		if _, err := p.Query(ctx, &query.RawQuery{Template: deep, Collections: targets}); err != nil {
			log.Fatalf("deep walk: %v", err)
		}
	}
	fmt.Printf("S3 recursive chain(20) x%d: avg=%v\n", rounds, time.Since(start)/rounds)

	// --- Item 9: pooled release vs model binding overhead ---
	// seed 2000 docs for a realistic scan
	bulk := make([]data.Documenter, 0, 2000)
	for i := 0; i < 2000; i++ {
		d := must(data.NewDocument(map[string]any{"name": fmt.Sprintf("bulk%d", i)}))
		bulk = append(bulk, d)
		if len(bulk) == 200 {
			if _, err := nodes.CreateMany(ctx, bulk); err != nil {
				log.Fatalf("bulk: %v", err)
			}
			bulk = bulk[:0]
		}
	}

	all := query.NewQueryBuilder().Limit(5000).Build()
	const scans = 20
	start = time.Now()
	for i := 0; i < scans; i++ {
		res, err := nodes.Read(ctx, &all)
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range res.Data {
			_ = d.ID()
			d.Release()
		}
	}
	rawAvg := time.Since(start) / scans

	mc := must(collection.NewModelCollection[*Node](nodes, zap.NewNop()))
	start = time.Now()
	for i := 0; i < scans; i++ {
		rows, err := mc.Read(ctx, &all)
		if err != nil {
			log.Fatal(err)
		}
		_ = len(rows)
	}
	modelAvg := time.Since(start) / scans
	fmt.Printf("S9 scan 2000 rows raw+Release: avg=%v; model bind: avg=%v; overhead=%v\n",
		rawAvg, modelAvg, modelAvg-rawAvg)
}

func must2(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func count(r *query.RawQueryResult) int {
	if r == nil {
		return -1
	}
	return r.Count
}

func dataLen(r *query.RawQueryResult) int {
	if r == nil || r.Data == nil {
		return 0
	}
	if rows, ok := r.Data.([]any); ok {
		return len(rows)
	}
	return -1
}

func qrData(r *query.RawQueryResult) any {
	if r == nil {
		return nil
	}
	return r.Data
}
