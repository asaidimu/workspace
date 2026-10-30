package anansistore

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// timeFormat is fixed-width (always 9 fractional digits, always Z) so that
// lexicographic order over stored strings equals chronological order — the
// keyset filters on created_at/updated_at push down as string comparisons.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

// bigLimit is the explicit "no limit" page size for reads. Probe showed
// an unbounded Read already returns every row, but the limit is set anyway
// so the adapter never depends on an implementation default.
const bigLimit = 1_000_000

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// parseTime inverts fmtTime; "" maps to the zero time so a zero clock value
// round-trips as zero (and not as year 1 in UTC, which would break DeepEqual
// against the memory store).
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		if t, err = time.Parse(time.RFC3339Nano, s); err != nil {
			return time.Time{}
		}
	}
	return t
}

// --- query building ---

func condStr(field string, op query.ComparisonOperator, val string) query.QueryFilter {
	v := val
	return query.QueryFilter{Condition: &query.FilterCondition{
		Field: field, Operator: op, Value: query.FilterValue{StringVal: &v},
	}}
}

func eq(field, val string) query.QueryFilter {
	return condStr(field, query.ComparisonOperatorEq, val)
}

// newQuery builds a query from top-level filters ANDed together (the builder
// ANDs chained Where/AndFilter calls), with an explicit limit.
func newQuery(limit int, filters ...query.QueryFilter) query.Query {
	b := query.NewQueryBuilder()
	for _, f := range filters {
		b = b.AndFilter(f)
	}
	if limit <= 0 {
		limit = bigLimit
	}
	return b.Limit(limit).Build()
}

// readDocs runs q against coll, calls fn for each row (fn must not retain
// the document), and releases the pooled documents.
func readDocs(ctx context.Context, coll base.Collection, q query.Query, fn func(d data.Documenter) error) error {
	res, err := coll.Read(ctx, &q)
	if err != nil {
		return translate(err)
	}
	defer func() {
		for _, d := range res.Data {
			d.Release()
		}
	}()
	for _, d := range res.Data {
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

// firstDoc reads a single row matching the filter, or reports found=false.
func firstDoc(ctx context.Context, coll base.Collection, filter query.QueryFilter) (data.Documenter, error) {
	q := newQuery(1, filter)
	res, err := coll.Read(ctx, &q)
	if err != nil {
		return nil, translate(err)
	}
	if len(res.Data) == 0 {
		return nil, nil
	}
	return res.Data[0], nil
}

// releaseDocs releases a document slice (used where firstDoc's result must
// outlive a readDocs-style loop).
func releaseDocs(docs []data.Documenter) {
	for _, d := range docs {
		d.Release()
	}
}

// --- document getters ---

func sget(d data.Documenter, key string) string {
	s, err := d.GetString(key)
	if err != nil {
		return ""
	}
	return s
}

func iget(d data.Documenter, key string) int {
	n, err := d.GetInt(key)
	if err != nil {
		return 0
	}
	return n
}

func bget(d data.Documenter, key string) bool {
	b, err := d.GetBool(key)
	if err != nil {
		return false
	}
	return b
}

// revOf reads the anansi metadata version — the domain Rev. A missing or
// unreadable version is corruption: the adapter always reads rows that
// anansi itself enriched.
func revOf(d data.Documenter) int64 {
	v, err := d.Version()
	if err != nil {
		return 0
	}
	return int64(v)
}

// --- domain JSON helpers ---

func encodeJSON(v any) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// decodeJSONMap unmarshals an optional *_json field: absent key -> nil map
// (round-trips a nil), "[]" -> empty non-nil slice, etc.
func decodeJSONBytes(d data.Documenter, key string, out any) error {
	if !d.HasKey(key) {
		return nil
	}
	return unmarshalStrict([]byte(sget(d, key)), out)
}

// unmarshalStrict decodes with UseNumber so big integers in free-form JSON
// (tool arguments, run state) keep their exact digits instead of becoming
// float64.
func unmarshalStrict(b []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return store.ErrCorrupt
	}
	return nil
}

// rawOf returns the opaque bytes of a *raw field: absent -> nil (so a nil
// json.RawMessage round-trips as nil), present -> exact bytes.
func rawOf(d data.Documenter, key string) json.RawMessage {
	if !d.HasKey(key) {
		return nil
	}
	return json.RawMessage(sget(d, key))
}

// stringsOf decodes an optional string-list field with nil/empty fidelity.
func stringsOf(d data.Documenter, key string) ([]string, error) {
	if !d.HasKey(key) {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(sget(d, key)), &out); err != nil {
		return nil, store.ErrCorrupt
	}
	return out, nil
}

// refsOf decodes an optional []domain.ContentRef field.
func refsOf(d data.Documenter, key string) ([]domain.ContentRef, error) {
	if !d.HasKey(key) {
		return nil, nil
	}
	var out []domain.ContentRef
	if err := unmarshalStrict([]byte(sget(d, key)), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- port semantics copied from store/memory (the oracle) ---

// inScope mirrors memory: System sees everything, otherwise the owner must
// match exactly.
func inScope(scope store.Scope, owner string) bool {
	if scope.System {
		return true
	}
	return scope.Owner == owner
}

// topicsMatch mirrors memory's topicsMatch: Any is a union, All an
// intersection, both together are ANDed, and a topic-less record matches
// only an empty filter.
func topicsMatch(topics []string, f store.TopicFilter) bool {
	have := make(map[string]bool, len(topics))
	for _, t := range topics {
		have[t] = true
	}
	if len(f.Any) > 0 {
		ok := false
		for _, want := range f.Any {
			if have[want] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, want := range f.All {
		if !have[want] {
			return false
		}
	}
	return true
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// paginate mirrors memory's keyset page exactly: sort ascending by key,
// drop keys <= cursor, take the window, set NextCursor when more rows exist,
// and only then reverse for NewestFirst — so the cursor is always the
// highest key of the window.
func paginate[T any](items []T, key func(T) string, limit int, cursor string, order store.Order) store.Page[T] {
	sort.SliceStable(items, func(i, j int) bool {
		return key(items[i]) < key(items[j])
	})
	if cursor != "" {
		n := 0
		for _, it := range items {
			if key(it) > cursor {
				items[n] = it
				n++
			}
		}
		items = items[:n]
	}
	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	page := store.Page[T]{Items: items[:limit]}
	if limit < len(items) {
		page.NextCursor = key(items[limit-1])
	}
	if order == store.NewestFirst {
		for i, j := 0, len(page.Items)-1; i < j; i, j = i+1, j-1 {
			page.Items[i], page.Items[j] = page.Items[j], page.Items[i]
		}
	}
	return page
}

func sortTurnRefs(refs []domain.TurnRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Version < refs[j].Version
	})
}

// alreadyExistsCollection recognizes anansi's duplicate-collection error so
// ensureCollection stays idempotent under a concurrent init.
func alreadyExistsCollection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "COLLECTION_ALREADY_EXISTS") ||
		(strings.Contains(msg, "already exists") && strings.Contains(msg, "collection"))
}

// pf takes the address of a filter value for the *query.QueryFilter fields
// of CollectionUpdate and Delete.
func pf(f query.QueryFilter) *query.QueryFilter { return &f }

// rdPtr returns a ReturnDocument flag. anansi only populates
// CollectionUpdateResult.Count from the returned rows, so every update
// whose Count we check must request the document back.
func rdPtr() *bool {
	v := true
	return &v
}
