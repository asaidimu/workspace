// Spike: Store specification section 15, items 4, 11, 12, 13.
//
//  4. Whether record fields preserve exact numbers and unknown fields.
//  11. Whether the document ID can be chosen by the caller, so (id, version)
//     can be the key and the model cache applies to turns.
//  12. Whether a projection shape read works through ReadAs (column pruning is
//     observed structurally; SQL-level pruning needs a separate check).
//  13. How the model cache behaves around a rolled-back transaction.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/asaidimu/go-anansi/v8"
	"github.com/asaidimu/go-anansi/v8/core/cache"
	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/document"
	"github.com/asaidimu/go-anansi/v8/core/persistence/collection"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

const recSchemaJSON = `{
  "version": "1.0.0",
  "name": "Rec",
  "fields": {
    "label":   { "name": "label",   "type": "string", "required": true },
    "payload": { "name": "payload", "type": "record", "required": false },
    "note":    { "name": "note",    "type": "string", "required": false }
  },
  "metadata": {
    "projections": {
      "RecLabel": { "include": ["label"] }
    }
  }
}`

type Rec struct {
	document.DocumentModel
	Label   string         `anansi:"label"`
	Payload map[string]any `anansi:"payload"`
	Note    string         `anansi:"note"`
}

type RecLabel struct {
	document.DocumentModel
	Label string `anansi:"label"`
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func main() {
	ctx := context.Background()
	sch := must(definition.FromJSON([]byte(recSchemaJSON)))
	p, cleanup, err := anansi.Playground(anansi.PlaygroundConfig{
		EnableLogging: false,
		EnableEvents:  false,
		Schemas:       []*definition.Schema{sch},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	raw := must(p.Collection(ctx, "Rec"))
	logger := zap.NewNop()
	mc := must(collection.NewModelCollection[*Rec](raw, logger, collection.ModelCollectionOptions[*Rec]{
		CacheConfig: &cache.CacheConfig{MaxEntries: 64},
	}))
	recs := &struct {
		*collection.ModelCollection[*Rec]
	}{mc}

	// --- Item 4: exact numbers + unknown fields through a record field ---
	original := `{"big":9007199254740993,"small":0.1,"nested":{"x":[1,2.5,true,null]},"unk":"keep me","uni":"héllo é 日本語"}`
	var payload map[string]any
	if err := json.Unmarshal([]byte(original), &payload); err != nil {
		log.Fatal(err)
	}
	_, err = recs.Create(ctx, &Rec{Label: "num", Payload: payload})
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	q := query.NewQueryBuilder().Where("label").Eq("num").Limit(1).Build()
	got := must(recs.Read(ctx, &q))
	if len(got) == 0 {
		log.Fatal("num record missing")
	}
	back, err := json.Marshal(got[0].Payload)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("S4 record in : %s\n", original)
	fmt.Printf("S4 record out: %s\n", string(back))
	fmt.Printf("S4 exact big int preserved: %v\n", strings.Contains(string(back), "9007199254740993"))
	fmt.Printf("S4 unknown field preserved: %v\n", strings.Contains(string(back), "keep me"))

	// --- Item 11: caller-chosen document ID as (id, version) key ---
	turnKey := "0192fa7b000070008000000000000001#1"
	doc, err := data.NewDocument(map[string]any{
		"_id_":    turnKey,
		"label":   "turn",
		"note":    "chosen id",
		"payload": map[string]any{"a": 1},
	})
	if err != nil {
		log.Fatal(err)
	}
	res, err := raw.CreateOne(ctx, doc)
	if err != nil {
		log.Fatalf("create with chosen id: %v", err)
	}
	fmt.Printf("S11 caller-chosen id honored: %v\n", res.Data.ID() == turnKey)
	backQ := query.NewQueryBuilder().Where(data.DocumentIDField).Eq(turnKey).Limit(1).Build()
	br := must(raw.Read(ctx, &backQ))
	fmt.Printf("S11 read back by composite id: count=%d\n", br.Count)
	for _, d := range br.Data {
		d.Release()
	}

	// --- Item 13: model cache across a rolled-back transaction ---
	_, err = recs.Create(ctx, &Rec{Label: "cache", Note: "before"})
	if err != nil {
		log.Fatal(err)
	}
	cq := query.NewQueryBuilder().Where("label").Eq("cache").Limit(1).Build()
	cached := must(recs.Read(ctx, &cq))
	if len(cached) == 0 {
		log.Fatal("cache doc missing")
	}
	cid := cached[0].ID
	fmt.Printf("S13 baseline note=%q id=%s\n", cached[0].Note, cid)

	// update through the MODEL collection inside a transaction that rolls back
	_, err = raw.Transact(ctx, func(tctx context.Context) (any, error) {
		if _, uerr := recs.Update(tctx, cid, &Rec{Label: "cache", Note: "inside-tx"}); uerr != nil {
			return nil, uerr
		}
		return nil, fmt.Errorf("force rollback")
	})
	fmt.Printf("S13 tx rolled back: %v\n", err != nil)

	// FindByID is the cache-hit path (Read-by-query never consults the cache)
	after, ferr := recs.FindByID(ctx, cid)
	if ferr != nil {
		fmt.Printf("S13 FindByID after rollback: err=%v\n", ferr)
	} else {
		fmt.Printf("S13 FindByID note after rolled-back tx: %q (poisoned cache if not %q)\n", after.Note, "before")
	}

	// raw read (bypasses model cache) shows committed truth
	freshQ := query.NewQueryBuilder().Where(data.DocumentIDField).Eq(cid).Limit(1).Build()
	fr := must(raw.Read(ctx, &freshQ))
	for _, d := range fr.Data {
		note, _ := d.GetString("note")
		fmt.Printf("S13 raw note after rolled-back tx: %q (want %q)\n", note, "before")
		d.Release()
	}

	// --- Item 12: projection shape via ReadAs ---
	lq := query.NewQueryBuilder().Where("label").Eq("cache").Build()
	labels := must(recs.ReadAs[*RecLabel](ctx, &lq))
	if len(labels) > 0 {
		fmt.Printf("S12 ReadAs projection: %d rows, label=%q (type carries no Note field)\n", len(labels), labels[0].Label)
	} else {
		fmt.Println("S12 ReadAs projection: 0 rows")
	}
}
