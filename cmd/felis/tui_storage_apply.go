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

// It follows applyReverseProxy's write-config, apply-secret, roll chain, with the default "felis" namespace hardcoded as the rest of the wizard does.
func applyStorageConfig(ctx context.Context, method storageMethod, in s3Inputs) error {
	var uploadsCtx string
	var s3cfg config.RegistryS3Config
	if method == storageS3 {
		// Validate bucket access before changing config, Secrets, or deployments.
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
	// Persist host credentials and create the S3 Secret before restarting its consumers.
	if method == storageS3 {
		for _, c := range []struct{ path, value string }{
			{hostUploadsS3AccessKeyPath, in.accessKey},
			{hostUploadsS3SecretKeyPath, in.secretKey},
		} {
			if err := writeHostCredential(c.path, c.value); err != nil {
				return fmt.Errorf("keep the bucket credentials in %s: %w", c.path, err)
			}
		}
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

// Clear the S3 subtable when switching to local uploads so no stale coordinates remain.
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

// Pipe rendered credentials to kubectl instead of exposing them in argv.
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
