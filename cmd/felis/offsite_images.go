package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/registrygate"
	"felis.lolicon.best/internal/registryprune"
)

// registryImages is the platform registry as the off-site copy sees it: read
// anonymously through the gate (the catalog, its manifest index, manifests and
// blobs) and, for a restore, written as the platform principal. Both go through
// the node's loopback hostPort, the way the installer pushes.
type registryImages struct {
	host   string
	index  *registryprune.Client
	source *imagepush.Source
	pusher *imagepush.Pusher
}

func newRegistryImages(endpoint string) *registryImages {
	return &registryImages{
		host:   endpoint,
		index:  &registryprune.Client{Endpoint: "http://" + endpoint},
		source: &imagepush.Source{Scheme: "http"},
	}
}

func (r *registryImages) Repositories(ctx context.Context) ([]string, error) {
	return r.index.Repositories(ctx)
}

func (r *registryImages) Revisions(ctx context.Context, repo string) ([]string, map[string]string, error) {
	idx, err := r.index.Index(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	digests := make([]string, 0, len(idx.Revisions))
	for _, rev := range idx.Revisions {
		digests = append(digests, rev.Digest)
	}
	return digests, idx.Tags, nil
}

func (r *registryImages) Manifest(ctx context.Context, repo, digest string) ([]byte, string, error) {
	body, mt, err := r.source.Manifest(ctx, r.host, repo, digest)
	return body, mt, registryGone(err)
}

func (r *registryImages) Blob(ctx context.Context, repo, digest string) (io.ReadCloser, error) {
	rc, err := r.source.Blob(ctx, r.host, repo, digest)
	return rc, registryGone(err)
}

func (r *registryImages) PutBlob(ctx context.Context, repo, digest string, size int64, open func() (io.ReadCloser, error)) error {
	return r.pusher.UploadBlob(ctx, r.host, repo, digest, size, open)
}

func (r *registryImages) PutManifest(ctx context.Context, repo, reference, mediaType string, body []byte) error {
	_, err := r.pusher.PutManifest(ctx, r.host, repo, reference, mediaType, body)
	return err
}

// registryGone marks a 404 as a manifest or blob the registry no longer holds.
func registryGone(err error) error {
	var se *imagepush.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return fmt.Errorf("%w: %v", offsite.ErrImageGone, err)
	}
	return err
}

// offsiteRegistryEndpoint is where the host reaches the registry whose images
// the off-site copy covers: flag when given ("off" for none), otherwise the
// loopback hostPort of the in-cluster registry [registry] url names. A
// registry outside the cluster is not this install's to copy.
func offsiteRegistryEndpoint(flag string, reg config.RegistryConfig) string {
	switch flag {
	case "off":
		return ""
	case "":
	default:
		return flag
	}
	host, _, _ := strings.Cut(reg.URL, "/")
	name, _, _ := strings.Cut(host, ":")
	if strings.HasSuffix(name, ".svc") || strings.HasSuffix(name, ".svc.cluster.local") {
		return loopbackEndpoint(host)
	}
	return ""
}

// registryWriteCredential is the credential a host-side push uses:
// FELIS_REGISTRY_USERNAME/PASSWORD when set, otherwise the platform principal
// with REGISTRY_PLATFORM_TOKEN from the installer's secrets file.
func registryWriteCredential(secrets string) (string, string, error) {
	if err := loadEnvFile(secrets); err != nil {
		return "", "", fmt.Errorf("read %s: %w", secrets, err)
	}
	user, pass := os.Getenv("FELIS_REGISTRY_USERNAME"), os.Getenv("FELIS_REGISTRY_PASSWORD")
	if pass == "" {
		user, pass = registrygate.PrincipalPlatform, os.Getenv("REGISTRY_PLATFORM_TOKEN")
	}
	if pass == "" {
		return "", "", errors.New("no registry credential: set FELIS_REGISTRY_PASSWORD or run as root on the node (REGISTRY_PLATFORM_TOKEN in /etc/felis/secrets.env)")
	}
	return user, pass, nil
}

func offsiteFetchImages(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (the host copy)")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	registry := fs.String("registry", "", "host[:port] of the registry to push into (default: the loopback hostPort of the in-cluster registry)")
	secrets := fs.String("secrets-env", "/etc/felis/secrets.env", "installer secrets file holding REGISTRY_PLATFORM_TOKEN, read when FELIS_REGISTRY_PASSWORD is unset")
	at := fs.String("at", "", "registry index version to restore (default: the newest; `felis offsite list` shows them)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-images: %v\n", err)
		return 1
	}
	endpoint := offsiteRegistryEndpoint(*registry, cfg.Registry)
	if endpoint == "" {
		fmt.Fprintf(stderr, "felis offsite fetch-images: [registry] url %q is not the in-cluster registry; pass -registry host:port\n", cfg.Registry.URL)
		return 2
	}
	user, pass, err := registryWriteCredential(*secrets)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-images: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	stamp, idx, err := offsite.ChooseImageIndex(ctx, env.bucket, env.key, *at)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-images: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis offsite fetch-images: restoring registry index %s (%d repositories, %d images) into %s\n",
		stamp, len(idx.Repositories), idx.Images(), endpoint)
	target := newRegistryImages(endpoint)
	target.pusher = &imagepush.Pusher{Scheme: "http", Username: user, Password: pass}
	res, err := offsite.FetchImages(ctx, env.bucket, env.key, idx, target, stderr)
	fmt.Fprintf(stdout, "felis offsite fetch-images: %d of %d repositories restored, %d images, %d tags, %d blobs pushed (%s)\n",
		res.Repositories, len(idx.Repositories), res.Manifests, res.Tags, res.BlobsPushed, offsite.HumanBytes(res.BytesPushed))
	for _, f := range res.Failures {
		fmt.Fprintf(stderr, "felis offsite fetch-images: %s\n", f)
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-images: %v (a second run pushes only what is still missing)\n", err)
		return 1
	}
	return 0
}
