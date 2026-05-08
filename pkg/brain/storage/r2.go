// Package storage — r2.go: Cloudflare R2 driver via the S3 API.
// PILAR XXVI / 135.C.3.
//
// R2 implements BrainStore using aws-sdk-go-v2/service/s3 against
// `https://<account>.r2.cloudflarestorage.com`. The driver delegates
// large-object uploads to s3manager.Uploader (handles multipart >5 MiB
// transparently) and uses conditional PUT (`If-None-Match: *`) for the
// distributed lock so two nodes pushing simultaneously can't both
// claim the same lease.
//
// Tests live in r2_test.go and use s3API (a narrow interface satisfied
// by both the real s3.Client and an in-memory fake) so the driver is
// exercised without live R2 credentials. Real-credential smoke tests
// belong in `make brain-r2-smoke` and are not part of `go test ./...`.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// r2HTTPTimeout bounds every individual S3 operation. Multipart uploads
// chunk this internally so a 4 GiB push isn't subject to one timeout.
const r2HTTPTimeout = 60 * time.Second

// s3API is the minimum surface the driver consumes from s3.Client. The
// interface lets tests inject a fake implementation; production code
// satisfies it via a real *s3.Client. Method set is intentionally small
// so the test fake is light.
type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// R2Store is the Cloudflare R2 driver. Wraps an s3API client + bucket
// name. Use NewR2Store for the production constructor; tests construct
// directly with a fake client.
//
// `uploader` uses the (currently still functional) s3manager.Uploader
// API; aws-sdk-go-v2 has flagged it deprecated in favor of
// feature/s3/transfermanager — migration is a follow-up commit since
// transfermanager is not yet GA-stable in this dependency vendor cut.
type R2Store struct {
	api      s3API
	uploader *manager.Uploader //lint:ignore SA1019 s3manager.Uploader still usable; transfermanager migration pending
	bucket   string
}

// R2Credentials packs the four fields needed to authenticate against R2.
// Loaders populate it from `~/.neo/credentials.json` (provider="r2") or
// from environment variables in test/dev.
// [144.D] SecretAccessKey uses the Secret type so fmt/panic/log output never
// exposes the plaintext key via Stringer.
type R2Credentials struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey Secret
	Bucket          string
}

// NewR2StoreFromCredentials wires NewR2Store from a R2Credentials struct.
// Convenience for callers loading creds from pkg/auth keystore.
func NewR2StoreFromCredentials(c R2Credentials) (*R2Store, error) {
	return NewR2Store(c.AccountID, c.AccessKeyID, c.SecretAccessKey, c.Bucket)
}

// NewR2Store builds an R2Store from explicit credentials + endpoint.
// accountID, accessKeyID, bucket are all required; secretAccessKey is
// intentionally a Secret type to prevent panic/log credential exposure.
// The S3 endpoint is derived as https://<accountID>.r2.cloudflarestorage.com.
func NewR2Store(accountID, accessKeyID string, secretAccessKey Secret, bucket string) (*R2Store, error) {
	if accountID == "" || accessKeyID == "" || secretAccessKey.Reveal() == "" || bucket == "" {
		return nil, errors.New("R2Store: accountID, accessKeyID, secretAccessKey, bucket all required")
	}
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
	secret := secretAccessKey.Reveal() // only revealed inside this closure
	cfg := aws.Config{
		Region: "auto",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secret}, nil
		}),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true // R2 expects path-style for many operations
	})
	return &R2Store{
		api:      client,
		uploader: manager.NewUploader(client), //lint:ignore SA1019 see R2Store doc
		bucket:   bucket,
	}, nil
}

// Put uploads the body to s3://<bucket>/<key>. Uses multipart when the
// reader is large enough to justify the overhead.
func (s *R2Store) Put(key string, r io.Reader) (int64, error) {
	if key == "" {
		return 0, errors.New("Put: empty key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()

	if s.uploader != nil {
		out, err := s.uploader.Upload(ctx, &s3.PutObjectInput{ //lint:ignore SA1019 see R2Store doc
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   r,
		})
		if err != nil {
			return 0, fmt.Errorf("Put %q: %w", key, err)
		}
		_ = out // ETag available; not surfaced to caller
		return -1, nil // s3manager doesn't report bytes; -1 signals "size unknown"
	}
	// Test path: no uploader, buffer + single PutObject.
	buf, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("Put %q: read body: %w", key, err)
	}
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(buf),
		ContentLength: aws.Int64(int64(len(buf))),
	})
	if err != nil {
		return 0, fmt.Errorf("Put %q: %w", key, err)
	}
	return int64(len(buf)), nil
}

// Get streams s3://<bucket>/<key> back to the caller. Returns
// ErrNotFound when the object does not exist.
func (s *R2Store) Get(key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("Get: empty key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		// Some s3-compatible servers return a generic 404 wrapper.
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "404") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("Get %q: %w", key, err)
	}
	return out.Body, nil
}

// List enumerates objects under prefix. Pages through ListObjectsV2 with
// a 1000-item batch so prefix scans on large buckets still terminate.
func (s *R2Store) List(prefix string) ([]ChunkRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()

	var out []ChunkRef
	var continuationToken *string
	for {
		page, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
			MaxKeys:           aws.Int32(1000),
		})
		if err != nil {
			return nil, fmt.Errorf("List %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			ref := ChunkRef{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size)}
			if obj.ETag != nil {
				ref.ETag = strings.Trim(*obj.ETag, `"`)
			}
			if obj.LastModified != nil {
				ref.UpdatedAt = *obj.LastModified
			}
			out = append(out, ref)
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		continuationToken = page.NextContinuationToken
	}
	return out, nil
}

// Delete removes the object at key. R2 returns success even when the
// key did not exist, so this is naturally idempotent.
func (s *R2Store) Delete(key string) error {
	if key == "" {
		return errors.New("Delete: empty key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()
	_, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("Delete %q: %w", key, err)
	}
	return nil
}

// Lock acquires a distributed lock by writing a small JSON object at
// `locks/<name>.json` with `If-None-Match: *` (atomic create-only). If
// the object already exists and its ExpiresAt is in the future, the
// PUT fails with PreconditionFailed and we report ErrLockHeld. If the
// existing object is expired, we DELETE it first then retry the
// conditional PUT.
//
// OpaqueToken is the holder + nano timestamp; Unlock verifies it before
// deleting the lock object so two consecutive Locks by different
// holders don't accidentally undo each other's leases.
func (s *R2Store) Lock(name, holder string, ttl time.Duration) (Lease, error) {
	if name == "" || holder == "" {
		return Lease{}, errors.New("Lock: name and holder required")
	}
	if ttl <= 0 {
		return Lease{}, errors.New("Lock: ttl must be > 0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()

	key := "locks/" + sanitizeLockName(name) + ".json"
	lf := lockFile{
		Name:        name,
		Holder:      holder,
		ExpiresAt:   time.Now().Add(ttl),
		OpaqueToken: fmt.Sprintf("%s-%d", holder, time.Now().UnixNano()),
	}
	data, _ := json.Marshal(lf)

	// First attempt: conditional PUT, fails if object exists.
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return Lease(lf), nil
	}
	if !isPreconditionFailed(err) {
		return Lease{}, fmt.Errorf("Lock: %w", err)
	}
	// Lock exists. Read it to check expiry; capture ETag for conditional delete.
	existing, existingETag := s.readRemoteLock(ctx, key)
	if existing != nil && time.Now().Before(existing.ExpiresAt) {
		return Lease{}, fmt.Errorf("%w: held by %s until %s", ErrLockHeld, existing.Holder, existing.ExpiresAt.Format(time.RFC3339))
	}
	// Expired or unparseable — conditional delete narrows the TOCTOU window:
	// if another process reclaimed the lock between our read and this delete,
	// IfMatch fires 412 and we report ErrLockHeld instead of stomping their claim.
	reclaimDel := &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if existingETag != "" {
		reclaimDel.IfMatch = aws.String(existingETag)
	}
	if _, derr := s.api.DeleteObject(ctx, reclaimDel); derr != nil {
		if isPreconditionFailed(derr) {
			return Lease{}, fmt.Errorf("%w: reclaim race (object modified between read and delete)", ErrLockHeld)
		}
		return Lease{}, fmt.Errorf("Lock: reclaim delete: %w", derr)
	}
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		return Lease{}, fmt.Errorf("Lock: retry: %w", err)
	}
	return Lease(lf), nil
}

// Unlock removes locks/<name>.json after verifying the OpaqueToken matches.
func (s *R2Store) Unlock(lease Lease) error {
	if lease.Name == "" {
		return errors.New("Unlock: empty lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r2HTTPTimeout)
	defer cancel()
	key := "locks/" + sanitizeLockName(lease.Name) + ".json"
	existing, existingETag := s.readRemoteLock(ctx, key)
	if existing == nil || existing.OpaqueToken != lease.OpaqueToken {
		return nil // no-op (already gone or reclaimed)
	}
	// Conditional delete: if the object changed between our read and this delete
	// (another holder reclaimed the expired lease), IfMatch fires 412 and we
	// treat it as a no-op rather than deleting their valid claim.
	unlockDel := &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if existingETag != "" {
		unlockDel.IfMatch = aws.String(existingETag)
	}
	if _, err := s.api.DeleteObject(ctx, unlockDel); err != nil {
		if isPreconditionFailed(err) {
			return nil // object changed between read and delete — already gone or reclaimed
		}
		return fmt.Errorf("Unlock: %w", err)
	}
	return nil
}

// Close zeros the s3 client reference. The aws-sdk-go-v2 client itself
// has no explicit Close (it holds an http.Client which we don't own).
func (s *R2Store) Close() error {
	s.api = nil
	s.uploader = nil
	return nil
}

// readRemoteLock fetches + parses locks/<name>.json. Returns (nil, "") on any
// failure (caller treats as "no lock present"). The returned ETag is used for
// conditional deletes to narrow the TOCTOU window between read and delete.
func (s *R2Store) readRemoteLock(ctx context.Context, key string) (*lockFile, string) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, ""
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, 4096))
	if err != nil {
		return nil, ""
	}
	var lf lockFile
	if err := json.Unmarshal(body, &lf); err != nil {
		return nil, ""
	}
	etag := ""
	if out.ETag != nil {
		etag = strings.Trim(*out.ETag, `"`)
	}
	return &lf, etag
}

// isPreconditionFailed detects the 412 / If-None-Match failure across
// the various error shapes aws-sdk-go-v2 may produce.
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "PreconditionFailed") ||
		strings.Contains(msg, "412") ||
		strings.Contains(msg, "ConditionalRequestConflict")
}
