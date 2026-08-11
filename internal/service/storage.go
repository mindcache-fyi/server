// Package service implements the core business logic for mindcache storage,
// analysis, creation, and knowledge integration. Services are transport
// agnostic and know nothing about HTTP.
package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gocloud.dev/blob"

	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"
)

// Storage is a blob-backed object store addressed by slash-separated keys.
// The concrete backend (local filesystem, S3, in-memory) is selected by the
// URL passed to NewStorage, so switching deployment targets requires no code
// changes.
type Storage struct {
	bucket    *blob.Bucket
	localRoot string
}

// NewStorage opens the blob bucket identified by storageURL, for example
// "file://./data" or "s3://bucket?region=us-east-1". The caller must call
// Close to release the underlying bucket once it is no longer needed.
func NewStorage(ctx context.Context, storageURL string) (*Storage, error) {
	b, err := blob.OpenBucket(ctx, storageURL)
	if err != nil {
		return nil, err
	}
	s := &Storage{bucket: b}
	if strings.HasPrefix(storageURL, "file://") {
		s.localRoot = strings.TrimPrefix(storageURL, "file://")
	}
	return s, nil
}

// Write stores data under key, overwriting any existing object.
func (s *Storage) Write(ctx context.Context, key string, data []byte) error {
	return s.bucket.WriteAll(ctx, key, data, nil)
}

// Read returns the full contents of the object stored under key.
func (s *Storage) Read(ctx context.Context, key string) ([]byte, error) {
	return s.bucket.ReadAll(ctx, key)
}

// Exists reports whether an object exists under key.
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	return s.bucket.Exists(ctx, key)
}

// Delete removes the object stored under key.
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.bucket.Delete(ctx, key)
}

// DeleteDir removes every object whose key begins with prefix, effectively
// clearing a logical directory. For file:// backends, the empty directory
// is also removed from the filesystem.
func (s *Storage) DeleteDir(ctx context.Context, prefix string) error {
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := s.bucket.Delete(ctx, obj.Key); err != nil {
			return err
		}
	}
	if s.localRoot != "" {
		os.RemoveAll(filepath.Join(s.localRoot, prefix))
	}
	return nil
}

// Close releases the underlying bucket.
func (s *Storage) Close() error {
	return s.bucket.Close()
}

// IsAccessible reports whether the bucket can be reached and listed. An empty
// but reachable bucket is considered accessible.
func (s *Storage) IsAccessible(ctx context.Context) bool {
	_, err := s.bucket.List(&blob.ListOptions{}).Next(ctx)
	return err == nil || errors.Is(err, io.EOF)
}
