package content

import (
	"context"

	"github.com/asaidimu/workspace/provider"
)

// BlobResolver turns a block's blob reference into a provider part: either a
// media Source the adapter streams from, or a provider-side remote id when
// the blob has already been uploaded. It is a port so content never needs
// the store; the workspace root owns the implementation.
type BlobResolver interface {
	// Resolve looks a blob up in the owner's scope and returns the part the
	// providerName adapter consumes for it.
	Resolve(ctx context.Context, owner, ref, providerName string) (provider.Part, error)
}

// MapEnv is what a Mapper needs beyond the block itself: the resolver for
// blob references and the owner the read must be scoped to. Both can be
// empty — a test prompt assembling without a blob store, say — and a mapper
// treats an empty environment as "resolve nothing", keeping its downgrade
// placeholder.
type MapEnv struct {
	Blobs BlobResolver
	Owner string
}
