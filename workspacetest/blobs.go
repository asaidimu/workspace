package workspacetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"sync"

	"github.com/asaidimu/workspace/blob"
)

// MemoryBlobs is an in-memory blob.Store double. Bytes are stored under
// their sha256 checksum, so Put is content-addressed: the same bytes
// registered twice report the same checksum, which is what the real
// adapters do.
type MemoryBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
	info map[string]blob.Info
}

// NewMemoryBlobs returns an empty double.
func NewMemoryBlobs() *MemoryBlobs {
	return &MemoryBlobs{
		data: map[string][]byte{},
		info: map[string]blob.Info{},
	}
}

// Put reads r to the end, checksums it and keeps the bytes. The checksum is
// "sha256:<hex>", the form domain.Blob documents.
func (b *MemoryBlobs) Put(ctx context.Context, r io.Reader, opts blob.PutOptions) (blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return blob.Info{}, err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return blob.Info{}, err
	}
	sum := sha256.Sum256(raw)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	info := blob.Info{Checksum: checksum, Size: int64(len(raw)), MediaType: opts.MediaType}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[checksum] = raw
	b.info[checksum] = info
	return info, nil
}

// Open returns a reader over the stored bytes.
func (b *MemoryBlobs) Open(ctx context.Context, checksum string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	raw, ok := b.data[checksum]
	b.mu.Unlock()
	if !ok {
		return nil, errBlobNotFound
	}
	return readSeekCloser{bytes.NewReader(append([]byte(nil), raw...))}, nil
}

// Stat reports the stored metadata without moving bytes.
func (b *MemoryBlobs) Stat(ctx context.Context, checksum string) (blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return blob.Info{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	info, ok := b.info[checksum]
	if !ok {
		return blob.Info{}, errBlobNotFound
	}
	return info, nil
}

// Delete removes the bytes for a checksum.
func (b *MemoryBlobs) Delete(ctx context.Context, checksum string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.data[checksum]; !ok {
		return errBlobNotFound
	}
	delete(b.data, checksum)
	delete(b.info, checksum)
	return nil
}

// List returns checksums after the given one, in lexical order, capped at
// limit. An empty after starts at the beginning.
func (b *MemoryBlobs) List(ctx context.Context, after string, limit int) ([]blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	checksums := make([]string, 0, len(b.info))
	for sum := range b.info {
		if sum > after {
			checksums = append(checksums, sum)
		}
	}
	sort.Strings(checksums)
	if limit > 0 && len(checksums) > limit {
		checksums = checksums[:limit]
	}
	out := make([]blob.Info, 0, len(checksums))
	for _, sum := range checksums {
		out = append(out, b.info[sum])
	}
	return out, nil
}

// errBlobNotFound is this double's absence error. Like the package's other
// doubles it is package-local: a test asserts with errors.Is inside this
// package, or on the message outside it, rather than the package exporting
// an error hierarchy.
const errBlobNotFound scriptedError = "workspacetest: blob not found"

// readSeekCloser adapts a bytes.Reader to the interface the blob port
// returns; the bytes are already in memory, so Close releases nothing.
type readSeekCloser struct{ io.ReadSeeker }

func (readSeekCloser) Close() error { return nil }
