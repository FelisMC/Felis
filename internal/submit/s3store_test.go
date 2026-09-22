package submit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// fakeS3 is an in-memory s3Client: it records puts and returns a NoSuchKey/404
// error for a missing stat, so S3ContextStore's key derivation and not-found
// handling are exercised without a live bucket.
type fakeS3 struct {
	objects map[string][]byte
	putErr  error
	statErr error // when set, StatObject returns it (e.g. auth rejected / bucket missing)
}

func (f *fakeS3) PutObject(_ context.Context, bucket, object string, r io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	if f.putErr != nil {
		return minio.UploadInfo{}, f.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return minio.UploadInfo{}, err
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[bucket+"/"+object] = data
	return minio.UploadInfo{Bucket: bucket, Key: object, Size: int64(len(data))}, nil
}

func (f *fakeS3) StatObject(_ context.Context, bucket, object string, _ minio.StatObjectOptions) (minio.ObjectInfo, error) {
	if f.statErr != nil {
		return minio.ObjectInfo{}, f.statErr
	}
	if data, ok := f.objects[bucket+"/"+object]; ok {
		return minio.ObjectInfo{Key: object, Size: int64(len(data))}, nil
	}
	return minio.ObjectInfo{}, minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}
}

// fakeS3Object is the object handle fakeS3.GetObject yields: Stat mirrors
// StatObject's not-found behaviour, Read serves the stored bytes.
type fakeS3Object struct {
	data []byte
	err  error
}

func (o *fakeS3Object) Read(p []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	if len(o.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, o.data)
	o.data = o.data[n:]
	return n, nil
}

func (o *fakeS3Object) Close() error { return nil }

func (o *fakeS3Object) Stat() (minio.ObjectInfo, error) {
	if o.err != nil {
		return minio.ObjectInfo{}, o.err
	}
	return minio.ObjectInfo{Size: int64(len(o.data))}, nil
}

func (f *fakeS3) GetObject(_ context.Context, bucket, object string, _ minio.GetObjectOptions) (s3Object, error) {
	if data, ok := f.objects[bucket+"/"+object]; ok {
		return &fakeS3Object{data: append([]byte(nil), data...)}, nil
	}
	return &fakeS3Object{err: minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}}, nil
}

func TestCheckBucketAccess(t *testing.T) {
	ctx := context.Background()

	// A reachable bucket where the probe object is simply absent (NoSuchKey) — the
	// object-scoped key case that a bucket-level HeadBucket would wrongly reject.
	if err := checkBucketAccess(ctx, &fakeS3{}, "b"); err != nil {
		t.Fatalf("reachable bucket, probe absent = %v, want nil", err)
	}
	// A bare 404 (object absent, no bucket-specific code) is also success.
	if err := checkBucketAccess(ctx, &fakeS3{statErr: minio.ErrorResponse{StatusCode: http.StatusNotFound}}, "b"); err != nil {
		t.Fatalf("bare 404 = %v, want nil (object absent in a live bucket)", err)
	}
	// A missing bucket is a real error.
	if err := checkBucketAccess(ctx, &fakeS3{statErr: minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound}}, "b"); err == nil {
		t.Fatal("missing bucket = nil, want error")
	}
	// Rejected credentials are a real error.
	if err := checkBucketAccess(ctx, &fakeS3{statErr: minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}}, "b"); err == nil {
		t.Fatal("rejected credentials = nil, want error")
	}
	// An unreachable endpoint (non-HTTP error) is a real error.
	if err := checkBucketAccess(ctx, &fakeS3{statErr: errors.New("dial tcp: connection refused")}, "b"); err == nil {
		t.Fatal("unreachable endpoint = nil, want error")
	}
}

func TestCheckS3AccessValidatesConfigFirst(t *testing.T) {
	// A malformed base fails at construction, before any network probe is attempted.
	if err := CheckS3Access(context.Background(), S3StoreConfig{Base: "s3://", Endpoint: "x", AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Fatal("CheckS3Access with no bucket succeeded, want error")
	}
}

func TestS3ContextStorePutAndExists(t *testing.T) {
	fake := &fakeS3{}
	s := &S3ContextStore{client: fake, bucket: "felis-uploads", prefix: "builds"}
	ctx := context.Background()

	if ok, err := s.Exists(ctx, "sub-abc"); err != nil || ok {
		t.Fatalf("Exists before Put = (%v, %v), want (false, nil)", ok, err)
	}

	payload := "\x1f\x8b\x08\x00the modpack context"
	n, err := s.Put(ctx, "sub-abc", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put returned %d bytes, want %d", n, len(payload))
	}

	// The blob lands at exactly {prefix}/{id}/context.tar.gz — the key half of the
	// s3://bucket/prefix/id/context.tar.gz ref deriveContextRef records.
	wantKey := "felis-uploads/builds/sub-abc/" + contextBlobName
	if got := string(fake.objects[wantKey]); got != payload {
		t.Fatalf("object at %q = %q, want %q", wantKey, got, payload)
	}
	if ok, err := s.Exists(ctx, "sub-abc"); err != nil || !ok {
		t.Fatalf("Exists after Put = (%v, %v), want (true, nil)", ok, err)
	}
}

// Open serves the stored object's bytes and maps a missing key to ErrBlobNotFound
// (the internal fetch route's 404), eagerly — before the caller reads a byte.
func TestS3ContextStoreOpen(t *testing.T) {
	fake := &fakeS3{}
	s := &S3ContextStore{client: fake, bucket: "felis-uploads", prefix: "builds"}
	ctx := context.Background()

	if _, err := s.Open(ctx, "sub-gone"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("Open of a missing object = %v, want ErrBlobNotFound", err)
	}
	payload := "\x1f\x8b\x08\x00the modpack context"
	if _, err := s.Put(ctx, "sub-abc", strings.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Open(ctx, "sub-abc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("Open served %q, want %q", got, payload)
	}
}

func TestS3ContextStoreEmptyPrefix(t *testing.T) {
	fake := &fakeS3{}
	s := &S3ContextStore{client: fake, bucket: "b", prefix: ""}
	if _, err := s.Put(context.Background(), "sub-1", strings.NewReader("\x1f\x8bx")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// No prefix ⇒ the key is just {id}/context.tar.gz (no leading slash).
	if _, ok := fake.objects["b/sub-1/"+contextBlobName]; !ok {
		t.Fatalf("object not at expected key; got keys %v", s3KeysOf(fake.objects))
	}
}

func TestS3ContextStoreRejectsUnsafeID(t *testing.T) {
	fake := &fakeS3{}
	s := &S3ContextStore{client: fake, bucket: "b", prefix: "p"}
	ctx := context.Background()

	for _, id := range []string{"../evil", "sub/../../etc", "SUB-UPPER", "has space", "", "a/b"} {
		if _, err := s.Put(ctx, id, strings.NewReader("\x1f\x8bx")); err == nil {
			t.Errorf("Put(%q) succeeded, want rejection", id)
		}
		if _, err := s.Exists(ctx, id); err == nil {
			t.Errorf("Exists(%q) succeeded, want rejection", id)
		}
	}
	if len(fake.objects) != 0 {
		t.Fatalf("an unsafe id wrote an object: %v", s3KeysOf(fake.objects))
	}
}

func TestParseS3Base(t *testing.T) {
	cases := []struct {
		base           string
		bucket, prefix string
		wantErr        bool
	}{
		{"s3://felis-user-uploads", "felis-user-uploads", "", false},
		{"s3://bucket/builds", "bucket", "builds", false},
		{"s3://bucket/a/b/c", "bucket", "a/b/c", false},
		{"S3://Bucket/", "Bucket", "", false},
		{"s3://bucket/pre/", "bucket", "pre", false},
		{"s3://", "", "", true},
		{"s3:///onlyslash", "", "", false}, // trims to "onlyslash" bucket
	}
	for _, c := range cases {
		bucket, prefix, err := parseS3Base(c.base)
		if (err != nil) != c.wantErr {
			t.Errorf("parseS3Base(%q) err = %v, wantErr %v", c.base, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if c.base == "s3:///onlyslash" {
			if bucket != "onlyslash" {
				t.Errorf("parseS3Base(%q) bucket = %q, want onlyslash", c.base, bucket)
			}
			continue
		}
		if bucket != c.bucket || prefix != c.prefix {
			t.Errorf("parseS3Base(%q) = (%q, %q), want (%q, %q)", c.base, bucket, prefix, c.bucket, c.prefix)
		}
	}
}

func TestSplitS3Endpoint(t *testing.T) {
	cases := []struct {
		ep      string
		host    string
		secure  bool
		wantErr bool
	}{
		{"https://s3.amazonaws.com", "s3.amazonaws.com", true, false},
		{"http://minio:9000", "minio:9000", false, false},
		{"minio.example.com:9000", "minio.example.com:9000", true, false},
		{"https://s3.example.com/", "s3.example.com", true, false},
		{"", "", false, true},
	}
	for _, c := range cases {
		host, secure, err := splitS3Endpoint(c.ep)
		if (err != nil) != c.wantErr {
			t.Errorf("splitS3Endpoint(%q) err = %v, wantErr %v", c.ep, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if host != c.host || secure != c.secure {
			t.Errorf("splitS3Endpoint(%q) = (%q, %v), want (%q, %v)", c.ep, host, secure, c.host, c.secure)
		}
	}
}

func TestNewS3ContextStoreValidation(t *testing.T) {
	if _, err := NewS3ContextStore(S3StoreConfig{Base: "s3://", AccessKey: "a", SecretKey: "b", Endpoint: "x"}); err == nil {
		t.Error("NewS3ContextStore with no bucket succeeded, want error")
	}
	if _, err := NewS3ContextStore(S3StoreConfig{Base: "s3://b", AccessKey: "", SecretKey: "", Endpoint: "x"}); err == nil {
		t.Error("NewS3ContextStore with no credentials succeeded, want error")
	}
	if _, err := NewS3ContextStore(S3StoreConfig{Base: "s3://b", AccessKey: "a", SecretKey: "b", Endpoint: ""}); err == nil {
		t.Error("NewS3ContextStore with no endpoint succeeded, want error")
	}
	if _, err := NewS3ContextStore(S3StoreConfig{Base: "s3://b/pre", AccessKey: "a", SecretKey: "b", Endpoint: "minio:9000"}); err != nil {
		t.Errorf("NewS3ContextStore with valid config: %v", err)
	}
}

func s3KeysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
