// Package blob is the port for file bytes.
//
// Persistence of metadata lives in the store; bytes live here. The required
// order is always: write bytes, then register metadata and add holder links
// inside one store transaction. A crash between the two leaves orphan bytes,
// which a reconcile pass removes. Bytes are written before metadata, so
// metadata can never point at missing bytes.
package blob

import (
	"context"
	"io"
)

// Info describes stored bytes. Checksum is "sha256:<hex>" and is computed
// while streaming, so a Put never needs a second pass.
type Info struct {
	Checksum  string
	Size      int64
	MediaType string
}

// PutOptions carries the media type for a write.
type PutOptions struct {
	MediaType string
}

// Store is the byte store. Implementations are an adapter over
// asaidimu/blobs, a plain filesystem store for tests and small deployments,
// and an in-memory store.
type Store interface {
	Put(ctx context.Context, r io.Reader, opts PutOptions) (Info, error)
	Open(ctx context.Context, checksum string) (io.ReadSeekCloser, error)
	Stat(ctx context.Context, checksum string) (Info, error)
	Delete(ctx context.Context, checksum string) error
	List(ctx context.Context, after string, limit int) ([]Info, error)
}
