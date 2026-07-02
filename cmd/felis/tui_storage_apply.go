package main

import (
	"context"
	"fmt"
	"strings"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/submit"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// applyStorageConfig persists the operator's storage choice and rolls felis-api so
// it picks up the new backend. For local it stamps user_uploads_context at the
// uploads PVC mount; for S3 it stamps the s3:// base + the [registry.s3] endpoint
// and credential refs, and creates the felis-uploads-s3 Secret the deployment reads
// the keys from. It mirrors applyReverseProxy — the same write-config →
// apply-secret → roll chain, hardcoding the default "felis" namespace as the rest
// of the wizard does.
func applyStorageConfig(ctx context.Context, method storageMethod, in s3Inputs) error {
	var uploadsCtx string
	var s3cfg config.RegistryS3Config
	if method == storageS3 {
		// Preflight the coordinates BEFORE touching config, the Secret, or the
		// deployment: a mistyped key, wrong endpoint, or missing bucket fails here at
		// the keyboard instead of silently at the first real upload. Nothing has been
		// written yet, so a failed check leaves the install untouched.
		if err := submit.CheckS3Access(ctx, submit.S3StoreConfig{
			Base:      "s3://" + in.bucket,
			Endpoint:  in.endpoint,
			Region:    in.region,
			AccessKey: in.accessKey,
			SecretKey: in.secretKey,
		}); err != nil {
			return err
		}
		uploadsCtx = "s3://" + in.bucket
		s3cfg = config.RegistryS3Config{
			Endpoint:     in.endpoint,
			Region:       in.region,
			AccessKeyRef: platform.UploadsS3AccessKeyEnv,
			SecretKeyRef: platform.UploadsS3SecretKeyEnv,
		}
	} else {
		uploadsCtx = platform.UploadsLocalPath
	}

	if err := writeStorageConfig(uploadsCtx, s3cfg); err != nil {
		return err
	}
	// S3: land the credentials in their own Secret BEFORE the roll, so the optional
	// env refs resolve on the fresh pod. Local needs no Secret.
	if method == storageS3 {
		if err := applyUploadsS3Secret(ctx, in.accessKey, in.secretKey); err != nil {
			return err
		}
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
		return err
	}
	return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
}

// currentStorageInputs reads the storage backend already recorded in felis.toml so
// the reconfigure flow can pre-select the method and pre-fill the non-secret S3
// fields (endpoint/bucket/region). Credentials live only in the felis-uploads-s3
// Secret and are deliberately never read back — they must be re-entered to change.
// Any read error falls back to a blank local default rather than blocking reconfig.
func currentStorageInputs() (storageMethod, s3Inputs) {
	cfg, err := config.Load(hostSetupConfigPath)
	if err != nil {
		return storageLocal, s3Inputs{}
	}
	base := cfg.Registry.UserUploadsContext
	if !strings.HasPrefix(strings.ToLower(base), "s3://") {
		return storageLocal, s3Inputs{}
	}
	bucket := base[len("s3://"):]
	if i := strings.IndexByte(bucket, '/'); i >= 0 {
		bucket = bucket[:i]
	}
	return storageS3, s3Inputs{
		endpoint: cfg.Registry.S3.Endpoint,
		bucket:   bucket,
		region:   cfg.Registry.S3.Region,
	}
}

// writeStorageConfig stamps the uploads backend into both the host and pod config
// files. The S3 subtable is set for S3 and cleared (zero value) for local, so
// switching backends never leaves stale coordinates behind.
func writeStorageConfig(uploadsCtx string, s3cfg config.RegistryS3Config) error {
	for _, path := range []string{hostSetupConfigPath, podSetupConfigPath} {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.Registry.UserUploadsContext = uploadsCtx
		cfg.Registry.S3 = s3cfg
		if err := writeConfig(path, cfg); err != nil {
			return err
		}
	}
	return nil
}

// applyUploadsS3Secret creates (or replaces) the felis-uploads-s3 Secret the
// felis-api Deployment mounts the S3 credentials from. The Secret is rendered
// in-process and piped to `kubectl apply` — the keys are NEVER passed as
// command-line args, so they never appear in the host process table.
func applyUploadsS3Secret(ctx context.Context, accessKey, secretKey string) error {
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: platform.UploadsS3SecretName, Namespace: "felis"},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			platform.UploadsS3SecretAccessKey: accessKey,
			platform.UploadsS3SecretSecretKey: secretKey,
		},
	}
	manifest, err := yaml.Marshal(secret)
	if err != nil {
		return fmt.Errorf("render uploads s3 secret: %w", err)
	}
	return kubectlWithInput(ctx, manifest, "apply", "-f", "-")
}
