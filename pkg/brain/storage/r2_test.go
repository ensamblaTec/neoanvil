package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is an in-memory implementation of the s3API interface so tests
// can exercise R2Store without a live R2 bucket. The fake is small and
// dumb: it tracks objects in a map keyed by bucket+key, supports
// If-None-Match for the lock primitive, and reports NoSuchKey on missing
// objects so the driver's ErrNotFound mapping is exercised.
type fakeS3 struct {
	objects map[string][]byte
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func fkey(bucket, key string) string { return bucket + "/" + key }

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	k := fkey(aws.ToString(in.Bucket), aws.ToString(in.Key))
	if in.IfNoneMatch != nil && *in.IfNoneMatch == "*" {
		if _, exists := f.objects[k]; exists {
			return nil, errors.New("PreconditionFailed: object already exists")
		}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[k] = body
	return &s3.PutObjectOutput{ETag: aws.String(`"` + ksum(body) + `"`)}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	k := fkey(aws.ToString(in.Bucket), aws.ToString(in.Key))
	body, ok := f.objects[k]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: aws.Int64(int64(len(body))),
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	k := fkey(aws.ToString(in.Bucket), aws.ToString(in.Key))
	body, ok := f.objects[k]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, fkey(aws.ToString(in.Bucket), aws.ToString(in.Key)))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	bucketPrefix := aws.ToString(in.Bucket) + "/"
	prefix := aws.ToString(in.Prefix)
	var contents []s3types.Object
	for k, body := range f.objects {
		if !strings.HasPrefix(k, bucketPrefix) {
			continue
		}
		key := strings.TrimPrefix(k, bucketPrefix)
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		size := int64(len(body))
		now := time.Now()
		contents = append(contents, s3types.Object{
			Key:          aws.String(key),
			Size:         aws.Int64(size),
			ETag:         aws.String(`"` + ksum(body) + `"`),
			LastModified: aws.Time(now),
		})
	}
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(false)}, nil
}

// ksum is a tiny non-crypto checksum used for fake ETags. Just len().
func ksum(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	return "etag-" + string(rune('0'+(len(b)%10)))
}

// newFakeR2 returns an R2Store wired to an in-memory fake. uploader is
// nil, forcing the simple PutObject path (multipart not exercised here —
// covered separately in the s3manager smoke runbook).
func newFakeR2() (*R2Store, *fakeS3) {
	fake := newFakeS3()
	return &R2Store{api: fake, uploader: nil, bucket: "neo-brain"}, fake
}

// TestR2Store_PutGet_Roundtrip — happy path against fake.
func TestR2Store_PutGet_Roundtrip(t *testing.T) {
	s, _ := newFakeR2()
	body := []byte("test snapshot bytes")
	n, err := s.Put("snapshots/abc.tar.zst", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("Put returned %d, want %d", n, len(body))
	}
	rc, err := s.Get("snapshots/abc.tar.zst")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("got %q, want %q", got, body)
	}
}

// TestR2Store_Get_NotFound — missing key maps NoSuchKey to ErrNotFound.
func TestR2Store_Get_NotFound(t *testing.T) {
	s, _ := newFakeR2()
	_, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestR2Store_List_Prefix — List returns matching keys with sizes.
func TestR2Store_List_Prefix(t *testing.T) {
	s, _ := newFakeR2()
	for _, k := range []string{"snapshots/a.zst", "snapshots/b.zst", "manifests/m.json"} {
		if _, err := s.Put(k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List("snapshots/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Key, "snapshots/") {
			t.Errorf("leak: %q does not start with snapshots/", c.Key)
		}
	}
}

// TestR2Store_Delete_Idempotent — missing key returns nil; present key
// is removed.
func TestR2Store_Delete_Idempotent(t *testing.T) {
	s, _ := newFakeR2()
	if err := s.Delete("never-existed"); err != nil {
		t.Errorf("missing delete: %v", err)
	}
	if _, err := s.Put("k", bytes.NewReader([]byte("v"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Errorf("present delete: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete Get returned %v, want ErrNotFound", err)
	}
}

// TestR2Store_Lock_Exclusive — second Lock fails with ErrLockHeld.
func TestR2Store_Lock_Exclusive(t *testing.T) {
	s, _ := newFakeR2()
	lease1, err := s.Lock("push", "node-A", time.Minute)
	if err != nil {
		t.Fatalf("Lock 1: %v", err)
	}
	if _, err := s.Lock("push", "node-B", time.Minute); !errors.Is(err, ErrLockHeld) {
		t.Errorf("Lock 2 want ErrLockHeld, got %v", err)
	}
	if err := s.Unlock(lease1); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

// TestR2Store_Lock_Reclaim — expired lock is reclaimed by another holder.
func TestR2Store_Lock_Reclaim(t *testing.T) {
	s, fake := newFakeR2()
	if _, err := s.Lock("push", "node-A", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Manually backdate the existing lock so reclaim sees it as expired.
	k := fkey("neo-brain", "locks/push.json")
	var lf lockFile
	_ = json.Unmarshal(fake.objects[k], &lf)
	lf.ExpiresAt = time.Now().Add(-time.Hour)
	fake.objects[k], _ = json.Marshal(lf)

	lease, err := s.Lock("push", "node-B", time.Minute)
	if err != nil {
		t.Fatalf("reclaim Lock: %v", err)
	}
	if lease.Holder != "node-B" {
		t.Errorf("reclaimed holder = %q, want node-B", lease.Holder)
	}
}

// TestR2Store_Unlock_TokenMismatch — Unlock with a stale token is a
// no-op; current lock survives.
func TestR2Store_Unlock_TokenMismatch(t *testing.T) {
	s, _ := newFakeR2()
	if _, err := s.Lock("push", "node-A", time.Minute); err != nil {
		t.Fatal(err)
	}
	stale := Lease{Name: "push", OpaqueToken: "node-X-12345"}
	if err := s.Unlock(stale); err != nil {
		t.Errorf("token-mismatch Unlock should be no-op, got %v", err)
	}
	if _, err := s.Lock("push", "node-B", time.Minute); !errors.Is(err, ErrLockHeld) {
		t.Errorf("original lock vanished after token-mismatch Unlock: %v", err)
	}
}

// TestR2Store_Lock_RejectsBadInput — empty name/holder, non-positive ttl.
func TestR2Store_Lock_RejectsBadInput(t *testing.T) {
	s, _ := newFakeR2()
	cases := []struct{ name, holder string; ttl time.Duration }{
		{"", "h", time.Minute},
		{"n", "", time.Minute},
		{"n", "h", 0},
	}
	for _, c := range cases {
		if _, err := s.Lock(c.name, c.holder, c.ttl); err == nil {
			t.Errorf("bad input (name=%q holder=%q ttl=%v) should error", c.name, c.holder, c.ttl)
		}
	}
}

// TestR2Store_PreconditionFailed_Detection — the helper recognizes the
// shapes aws-sdk-go-v2 produces. Keep this list in sync with isPreconditionFailed.
func TestR2Store_PreconditionFailed_Detection(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("PreconditionFailed: ..."), true},
		{errors.New("HTTP 412 Precondition Failed"), true},
		{errors.New("ConditionalRequestConflict: ..."), true},
		{errors.New("NoSuchKey: ..."), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isPreconditionFailed(c.err); got != c.want {
			t.Errorf("isPreconditionFailed(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestNewR2Store_RejectsEmpty — accountID/keys/bucket all required.
func TestNewR2Store_RejectsEmpty(t *testing.T) {
	if _, err := NewR2Store("", "k", "s", "b"); err == nil {
		t.Error("empty accountID should error")
	}
	if _, err := NewR2Store("a", "", "s", "b"); err == nil {
		t.Error("empty accessKey should error")
	}
	if _, err := NewR2Store("a", "k", "", "b"); err == nil {
		t.Error("empty secret should error")
	}
	if _, err := NewR2Store("a", "k", "s", ""); err == nil {
		t.Error("empty bucket should error")
	}
}

// TestNewR2Store_BuildsClient — happy path constructs without error.
// Doesn't make any network calls.
func TestNewR2Store_BuildsClient(t *testing.T) {
	s, err := NewR2Store("acct", "k", "s", "b")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("nil store")
	}
	if s.bucket != "b" {
		t.Errorf("bucket = %q, want b", s.bucket)
	}
	_ = s.Close()
}
