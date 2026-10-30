// The workspace's BlobResolver: it turns the blob references inside blocks
// into provider parts. Owner scoping is the point — a ref is a blob ID and
// blob IDs are only meaningful inside their owner's tables — so Resolve
// takes the owner and reads the blob through the owner's repos. Media parts
// carry a lazily-reading Source, so bytes are opened only if the adapter
// sends the message; a provider-side remote id (set by an adapter after an
// upload) takes precedence when one is recorded.
package workspace

import (
	"context"
	"io"
	"strings"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Blobs returns the workspace's blob reference resolver: a content.BlobResolver
// backed by this workspace's store and blob store. Give it to a stepper
// config or a prompt Input; the content mappers consult it through their
// MapEnv.
func (w *Workspace) BlobResolver() BlobResolver { return blobResolver{w: w} }

// BlobResolver is the resolved resolver type the workspace hands out.
type BlobResolver interface {
	Resolve(ctx context.Context, owner, ref, providerName string) (provider.Part, error)
}

type blobResolver struct{ w *Workspace }

// Resolve implements content.BlobResolver (satisfied structurally: the
// signature matches content's port).
func (r blobResolver) Resolve(ctx context.Context, owner, ref, providerName string) (provider.Part, error) {
	if ref == "" {
		return nil, store.ErrNotFound
	}
	repos, err := r.w.store.Scoped(store.Scope{Owner: owner})
	if err != nil {
		return nil, err
	}
	row, err := repos.Blobs().Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Repositories' by-ID Gets are not owner-filtered (both stores mirror
	// that quirk, documented in store/anansistore/conversations.go), so the
	// resolver enforces the Store specification's owner isolation here.
	if row.Owner != owner {
		return nil, store.ErrNotFound
	}
	if remote, ok := row.Remotes[providerName]; ok {
		return remotePart(row, remote), nil
	}
	src := blobSource{w: r.w, checksum: row.Checksum, size: row.Size, media: mediaType(row)}
	if strings.HasPrefix(src.media, "image/") {
		return provider.ImagePart{Source: src}, nil
	}
	return provider.FilePart{Name: row.Name, Source: src}, nil
}

// remotePart builds the part for a blob the provider already holds. The
// remote's media kind matches the metadata kind by construction: only same
// kind uploads are recorded on a blob.
func remotePart(row domain.Blob, remote domain.RemoteRef) provider.Part {
	if strings.HasPrefix(mediaType(row), "image/") {
		return provider.ImagePart{RemoteID: remote.ID}
	}
	return provider.FilePart{Name: row.Name, RemoteID: remote.ID}
}

func mediaType(row domain.Blob) string {
	if row.MediaType == "" {
		return "application/octet-stream"
	}
	return row.MediaType
}

// blobSource is a provider.Source over the blob store: Open streams the
// bytes of one checksum.
type blobSource struct {
	w        *Workspace
	checksum string
	size     int64
	media    string
}

func (s blobSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return s.w.blobs.Open(ctx, s.checksum)
}
func (s blobSource) Size() int64       { return s.size }
func (s blobSource) MediaType() string { return s.media }
