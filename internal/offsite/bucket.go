package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Object is one object in the bucket, its key relative to the configured
// prefix.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Bucket is the object store the sync writes to. S3 is the production one; the
// tests use an in-memory map.
type Bucket interface {
	// Put stores exactly size bytes read from r under key.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens key. A missing key is ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	Remove(ctx context.Context, key string) error
}

// ErrNotFound is a key the bucket does not hold.
var ErrNotFound = errors.New("offsite: no such object")

// S3Config locates an S3-compatible bucket. Endpoint takes an http:// or
// https:// scheme; a bare host means TLS.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

// S3 is a Bucket on an S3-compatible store (AWS S3, Backblaze B2, Cloudflare
// R2, Wasabi, MinIO...). Every key is placed under Prefix.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// partSize bounds what one multipart upload holds in memory.
const partSize = 16 << 20

// NewS3 builds the client. It does not touch the network; Check does.
func NewS3(c S3Config) (*S3, error) {
	host, secure, err := splitEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	if c.Bucket == "" {
		return nil, errors.New("offsite: no bucket configured")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("offsite: the access key or the secret key is empty")
	}
	cl, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
		Region: c.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("offsite: %w", err)
	}
	return &S3{client: cl, bucket: c.Bucket, prefix: cleanPrefix(c.Prefix)}, nil
}

func splitEndpoint(ep string) (host string, secure bool, err error) {
	ep = strings.TrimSpace(ep)
	switch {
	case ep == "":
		return "", false, errors.New("offsite: no endpoint configured")
	case strings.HasPrefix(ep, "https://"):
		return strings.Trim(strings.TrimPrefix(ep, "https://"), "/"), true, nil
	case strings.HasPrefix(ep, "http://"):
		return strings.Trim(strings.TrimPrefix(ep, "http://"), "/"), false, nil
	default:
		return strings.Trim(ep, "/"), true, nil
	}
}

func cleanPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Check proves the bucket is reachable and the credentials may use it.
func (s *S3) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("offsite: cannot reach bucket %q (endpoint unreachable or credentials rejected): %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("offsite: bucket %q does not exist; create it first", s.bucket)
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    partSize,
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.prefix+key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy; Stat surfaces a missing key before the first read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, err
	}
	return obj, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix + prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		out = append(out, Object{Key: strings.TrimPrefix(o.Key, s.prefix), Size: o.Size, Modified: o.LastModified})
	}
	return out, nil
}

func (s *S3) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.prefix+key, minio.RemoveObjectOptions{})
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}
