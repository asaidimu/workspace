package prompt

import (
	"context"
	"sort"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Estimator is the default Tokenizer: bytes divided by four, rounded up, per
// part. It is deliberately cheap and deliberately wrong by a little, so every
// count it produces is recorded as approximate in the build's diagnostics.
type Estimator struct{}

// Count implements Tokenizer.
func (Estimator) Count(model string, parts []provider.Part) (int, error) {
	total := 0
	for _, p := range parts {
		total += estimatePart(p)
	}
	return total, nil
}

func estimatePart(p provider.Part) int {
	var n int
	switch v := p.(type) {
	case provider.TextPart:
		n = len(v.Text)
	case provider.ThinkingPart:
		n = len(v.Text)
	case provider.ToolCallPart:
		n = len(v.Name) + len(v.Arguments)
	case provider.ToolResultPart:
		n = len(v.Output)
	default:
		// Image, file and any future part: charge a small flat cost rather
		// than guessing at the bytes.
		return 4
	}
	if n == 0 {
		return 1
	}
	return (n + 3) / 4
}

// TopicRetriever is the default Retriever: it ranks by topic overlap with the
// conversation, then by recency, with a configurable limit (default 20).
// Full-text and embedding retrievers plug in behind the same interface; the
// TypeScript library defined Retriever but never implemented it.
type TopicRetriever struct {
	// Items is the corpus to rank. A production host loads it from the
	// store; tests pass a fixed slice so nothing touches a clock or a DB.
	Items []domain.Content
	// Limit bounds the result; zero falls back to the query's limit, then 20.
	Limit int
}

// scoredContent is one ranked item plus the recency key the sort uses.
type scoredContent struct {
	ref     store.ContentRef
	score   float64
	recency time.Time
	key     string
	overlap int
	total   int
}

// Retrieve implements Retriever.
func (r TopicRetriever) Retrieve(ctx context.Context, q RetrievalQuery) ([]Scored, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := r.Limit
	if limit <= 0 {
		limit = q.Limit
	}
	if limit <= 0 {
		limit = 20
	}

	topics := make(map[string]bool, len(q.Topics))
	for _, t := range q.Topics {
		topics[t] = true
	}

	var ranked []scoredContent
	for _, c := range r.Items {
		if c.Tombstoned || c.Owner != q.Owner {
			continue
		}
		overlap := 0
		for _, t := range c.Topics {
			if topics[t] {
				overlap++
			}
		}
		// Score is the share of the item's topics the conversation shares, so
		// a narrowly matching item outranks a broad one that happens to
		// mention a common topic.
		score := 0.0
		if len(c.Topics) > 0 {
			score = float64(overlap) / float64(len(c.Topics))
		}
		ranked = append(ranked, scoredContent{
			ref:     c.Ref(),
			score:   score,
			recency: c.CreatedAt,
			key:     c.Key,
			overlap: overlap,
			total:   len(c.Topics),
		})
	}

	// Rank by overlap, then recency. Key breaks the final tie so the order
	// never depends on the slice's construction or on the clock.
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.recency.Equal(b.recency) {
			return a.recency.After(b.recency)
		}
		return a.key < b.key
	})

	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Scored, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, Scored{Ref: s.ref, Score: s.score})
	}
	return out, nil
}
