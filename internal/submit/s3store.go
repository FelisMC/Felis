package submit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Client is the minimal object-store surface S3ContextStore needs. *minio.Client
// satisfies it through minioStoreClient, and a fake satisfies it in tests — so the
// store's key derivation and not-found handling are unit-verifiable without a live
// bucket.
type s3Client interface {
	PutObject(ctx context.Context, bucket, object string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	StatObject(ctx context.Context, bucket, object string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	GetObject(ctx context.Context, bucket, object string, opts minio.GetObjectOptions) (s3Object, error)
	RemoveObject(ctx context.Context, bucket, object string, opts minio.RemoveObjectOptions) error
}

// s3Object is the handle GetObject yields: a stream whose Stat performs the HEAD
// eagerly, so a missing object surfaces before the first byte is read.
type s3Object interface {
	io.ReadCloser
	Stat() (minio.ObjectInfo, error)
}

// minioStoreClient adapts *minio.Client to s3Client. The adapter exists because a
// method's return type cannot be narrowed by an interface: GetObject on the real
// client returns a concrete *minio.Object, which does not satisfy a method declared
// to return s3Object.
type minioStoreClient struct{ *minio.Client }

func (m minioStoreClient) GetObject(ctx context.Context, bucket, object string, opts minio.GetObjectOptions) (s3Object, error) {
	return m.Client.GetObject(ctx, bucket, object, opts)
}

// S3ContextStore is the object-store-backed build-context blob store: it writes
// each submission's uploaded modpack to {prefix}/{id}/context.tar.gz inside an S3
// bucket. It is the second implemented Blobs backend (alongside LocalContextStore),
// selected by cmd/felis when user_uploads_context is an s3:// base.
//
// The bucket + key prefix are parsed from that same base (parseS3Base), so an
// object written here lands at exactly s3://{bucket}/{prefix}/{id}/context.tar.gz.
// Credentials are static V4 keys resolved by cmd/felis from the environment (the
// setup wizard injects them into felis-api from the felis-uploads-s3 Secret); they
// never touch felis.toml.
//
// The sandboxed build Job never needs S3 credentials of its own: the api reads the
// object back here (Open) and streams it over the internal face, which is the
// transport every in-cluster build uses (Manager.ContextBaseURL). Only a
// deployment that leaves ContextBaseURL empty would fall back to Kaniko reading
// s3:// natively — and such a deployment would still need to hand the build Pod
// credentials + egress itself.
type S3ContextStore struct {
	client s3Client
	bucket string
	prefix string // key prefix within the bucket; may be empty
}

// S3StoreConfig is the resolved input for NewS3ContextStore. Base is the s3://
// user_uploads_context (bucket + optional prefix are parsed from it, so the write
// path matches deriveContextRef); Endpoint may carry an http:// or https:// scheme
// (a bare host defaults to TLS); the keys come from the environment.
type S3StoreConfig struct {
	Base      string
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

// NewS3ContextStore builds a store backed by a real minio client. It fails fast
// when the base is malformed or the credentials are missing, so cmd/felis leaves
// Manager.Blobs nil (upload endpoint → 503) rather than wiring a store that cannot
// authenticate.
func NewS3ContextStore(cfg S3StoreConfig) (*S3ContextStore, error) {
	bucket, prefix, err := parseS3Base(cfg.Base)
	if err != nil {
		return nil, err
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("submit: s3 store requires credentials")
	}
	host, secure, err := splitS3Endpoint(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("submit: s3 endpoint: %w", err)
	}
	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("submit: s3 client: %w", err)
	}
	return &S3ContextStore{client: minioStoreClient{client}, bucket: bucket, prefix: prefix}, nil
}

// CheckS3Access verifies the S3 coordinates before they are committed to config:
// it builds a client from the entered endpoint/credentials and probes the bucket.
// It is the install-time preflight that turns a mistyped key, wrong endpoint, or
// missing bucket into an immediate, legible error at the keyboard instead of a 503
// at the first real upload.
func CheckS3Access(ctx context.Context, cfg S3StoreConfig) error {
	store, err := NewS3ContextStore(cfg)
	if err != nil {
		return err
	}
	return checkBucketAccess(ctx, store.client, store.bucket)
}

// checkBucketAccess probes the bucket with the SAME object-level HEAD the upload
// path uses (StatObject on a key that will not exist), not a bucket-level
// HeadBucket. This matters: a least-privilege key scoped to object Put/Get may lack
// s3:ListBucket, so a HeadBucket would falsely reject a key that uploads fine. A
// NoSuchKey/absent result means the endpoint is reachable and the credentials are
// accepted — exactly the runtime dependency Exists() relies on. Split from
// CheckS3Access so the error mapping is unit-testable against a fake.
func checkBucketAccess(ctx context.Context, client s3Client, bucket string) error {
	const probe = "felis-access-probe/does-not-exist"
	if _, err := client.StatObject(ctx, bucket, probe, minio.StatObjectOptions{}); err != nil {
		resp := minio.ToErrorResponse(err)
		switch resp.Code {
		case "NoSuchKey", "NotFound":
			return nil // reachable + authorized; the probe object is simply absent
		case "NoSuchBucket":
			return fmt.Errorf("submit: bucket %q not found", bucket)
		case "AccessDenied", "SignatureDoesNotMatch", "InvalidAccessKeyId":
			return fmt.Errorf("submit: s3 credentials rejected: %w", err)
		default:
			// A bare 404 with no bucket-specific code = object absent in a live bucket.
			if resp.StatusCode == http.StatusNotFound {
				return nil
			}
			return fmt.Errorf("submit: cannot reach s3 (endpoint unreachable or credentials rejected): %w", err)
		}
	}
	return nil // the probe object improbably exists — access clearly works
}

// keyFor derives the object key for a submission, re-validating the id at the
// storage boundary (the same defense-in-depth as LocalContextStore: a validated id
// carries no path separator, so it cannot alter the key layout).
func (s *S3ContextStore) keyFor(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("submit: invalid submission id %q", id)
	}
	return path.Join(s.prefix, id, contextBlobName), nil
}

// Put streams r to the derived object key. Size is unknown (the Manager hands us a
// size-capped reader), so it is uploaded with size -1 (multipart). PutObject is
// atomic from a reader's perspective — a partial upload never becomes a readable
// object — so a failed or oversize upload never replaces a good context. It
// returns the number of bytes stored.
func (s *S3ContextStore) Put(ctx context.Context, id string, r io.Reader) (int64, error) {
	key, err := s.keyFor(id)
	if err != nil {
		return 0, err
	}
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return 0, fmt.Errorf("submit: put context blob: %w", err)
	}
	return info.Size, nil
}

// Exists reports whether a context blob has been stored for id. Approve consults
// it so a submission whose context was never uploaded is refused BEFORE the CAS.
func (s *S3ContextStore) Exists(ctx context.Context, id string) (bool, error) {
	key, err := s.keyFor(id)
	if err != nil {
		return false, err
	}
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("submit: stat context blob: %w", err)
	}
	return true, nil
}

// Size reports the stored blob's size — the accounting read behind the per-user
// storage budget. A missing object is (0, false, nil), the same distinction
// Exists draws.
func (s *S3ContextStore) Size(ctx context.Context, id string) (int64, bool, error) {
	key, err := s.keyFor(id)
	if err != nil {
		return 0, false, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("submit: stat context blob: %w", err)
	}
	return info.Size, true, nil
}

// Delete removes the stored object for the withdrawn/deleted submission. S3's
// DELETE is idempotent — removing an absent key succeeds — which is exactly the
// contract the cleanup path needs on a retry.
func (s *S3ContextStore) Delete(ctx context.Context, id string) error {
	key, err := s.keyFor(id)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("submit: remove context blob: %w", err)
	}
	return nil
}

// Open returns the stored context blob for id — the read side of the transport the
// build Pod's fetch initContainer uses. minio's GetObject returns only once the
// server answered with an object (it surfaces NoSuchKey up front), so a missing
// object maps to ErrBlobNotFound right here and the route answers 404.
func (s *S3ContextStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	key, err := s.keyFor(id)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
		}
		return nil, fmt.Errorf("submit: open context blob: %w", err)
	}
	// minio.Object is lazy: the first Read triggers the GET and is where a missing
	// key actually surfaces, so stat it once here to translate that case eagerly
	// (the caller can then trust the io.ReadCloser belongs to a real object).
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
		}
		return nil, fmt.Errorf("submit: open context blob: %w", err)
	}
	return obj, nil
}

// isS3NotFound recognizes the "object is absent" outcome across S3
// implementations: a GET-shaped NoSuchKey code or a bare 404 from the HEAD that
// StatObject issues.
func isS3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

// splitS3Endpoint separates a configured endpoint into the host[:port] minio.New
// wants and a TLS flag. A bare host defaults to TLS (the safe default); an
// explicit http:// opts out for a plaintext dev store.
func splitS3Endpoint(ep string) (host string, secure bool, err error) {
	ep = strings.TrimSpace(ep)
	switch {
	case ep == "":
		return "", false, errors.New("empty endpoint")
	case strings.HasPrefix(ep, "https://"):
		return strings.Trim(strings.TrimPrefix(ep, "https://"), "/"), true, nil
	case strings.HasPrefix(ep, "http://"):
		return strings.Trim(strings.TrimPrefix(ep, "http://"), "/"), false, nil
	default:
		return strings.Trim(ep, "/"), true, nil
	}
}

// parseS3Base splits an s3://bucket[/prefix] base into its bucket and key prefix.
// It is the single source of truth for how a user_uploads_context s3:// base maps
// onto object storage, kept beside the store so the write path and deriveContextRef
// can never disagree about where the blob lands.
func parseS3Base(base string) (bucket, prefix string, err error) {
	rest := base
	if i := strings.Index(strings.ToLower(rest), "://"); i >= 0 {
		rest = rest[i+3:]
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", "", fmt.Errorf("submit: s3 base %q has no bucket", base)
	}
	parts := strings.SplitN(rest, "/", 2)
	bucket = parts[0]
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	return bucket, prefix, nil
}

// Compile-time proof that the object store satisfies the Blobs transport.
var _ Blobs = (*S3ContextStore)(nil)
