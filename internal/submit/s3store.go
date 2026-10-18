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

type s3Client interface {
	PutObject(ctx context.Context, bucket, object string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	StatObject(ctx context.Context, bucket, object string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	GetObject(ctx context.Context, bucket, object string, opts minio.GetObjectOptions) (s3Object, error)
	RemoveObject(ctx context.Context, bucket, object string, opts minio.RemoveObjectOptions) error
}

// s3Object.Stat forces existence checks before streaming a lazy object handle.
type s3Object interface {
	io.ReadCloser
	Stat() (minio.ObjectInfo, error)
}

// minioStoreClient adapts concrete GetObject return types to the narrower s3Object interface.
type minioStoreClient struct{ *minio.Client }

func (m minioStoreClient) GetObject(ctx context.Context, bucket, object string, opts minio.GetObjectOptions) (s3Object, error) {
	return m.Client.GetObject(ctx, bucket, object, opts)
}

// S3ContextStore uses environment-provided credentials; build Jobs fetch through the internal API without bucket credentials.
type S3ContextStore struct {
	client s3Client
	bucket string
	prefix string
}

// S3StoreConfig.Base fixes the bucket and prefix; bare endpoints default to TLS and credentials come from the environment.
type S3StoreConfig struct {
	Base      string
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

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

func CheckS3Access(ctx context.Context, cfg S3StoreConfig) error {
	store, err := NewS3ContextStore(cfg)
	if err != nil {
		return err
	}
	return checkBucketAccess(ctx, store.client, store.bucket)
}

// checkBucketAccess probes an absent object with HEAD because object-scoped keys may not permit bucket-level listing.
func checkBucketAccess(ctx context.Context, client s3Client, bucket string) error {
	const probe = "felis-access-probe/does-not-exist"
	if _, err := client.StatObject(ctx, bucket, probe, minio.StatObjectOptions{}); err != nil {
		resp := minio.ToErrorResponse(err)
		switch resp.Code {
		case "NoSuchKey", "NotFound":
			return nil
		case "NoSuchBucket":
			return fmt.Errorf("submit: bucket %q not found", bucket)
		case "AccessDenied", "SignatureDoesNotMatch", "InvalidAccessKeyId":
			return fmt.Errorf("submit: s3 credentials rejected: %w", err)
		default:
			if resp.StatusCode == http.StatusNotFound {
				return nil
			}
			return fmt.Errorf("submit: cannot reach s3 (endpoint unreachable or credentials rejected): %w", err)
		}
	}
	return nil
}

func (s *S3ContextStore) keyFor(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("submit: invalid submission id %q", id)
	}
	return path.Join(s.prefix, id, contextBlobName), nil
}

// s3PartSize bounds upload memory; minio's default unknown-length part can exceed the API's memory limit.
const s3PartSize = 8 << 20

const s3MaxObject = s3PartSize * 10000

// Put streams fixed-size parts and exposes a replacement only after complete success.
func (s *S3ContextStore) Put(ctx context.Context, id string, r io.Reader) (int64, error) {
	key, err := s.keyFor(id)
	if err != nil {
		return 0, err
	}
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{ContentType: "application/gzip", PartSize: s3PartSize})
	if err != nil {
		return 0, fmt.Errorf("submit: put context blob: %w", err)
	}
	var probe [1]byte
	n, rerr := io.ReadFull(r, probe[:])
	if n == 0 && rerr == io.EOF {
		return info.Size, nil
	}
	// Require EOF after minio's final supported part so an oversized stream cannot be silently stored truncated.
	if n > 0 {
		rerr = fmt.Errorf("submit: context blob exceeds the %d-byte multipart limit", int64(s3MaxObject))
	} else {
		rerr = fmt.Errorf("submit: put context blob: %w", rerr)
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return 0, fmt.Errorf("%w (removing the truncated object also failed: %v)", rerr, err)
	}
	return 0, rerr
}

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

// Size returns (0,false,nil) for absent objects.
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
	// Stat the lazy object before returning it so missing keys map to ErrBlobNotFound before the first caller read.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
		}
		return nil, fmt.Errorf("submit: open context blob: %w", err)
	}
	return obj, nil
}

// isS3NotFound accepts NoSuchKey and bare object-HEAD 404 responses.
func isS3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

// splitS3Endpoint defaults bare hosts to TLS; explicit http opts into plaintext.
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

var _ Blobs = (*S3ContextStore)(nil)
