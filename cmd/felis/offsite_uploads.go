package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/platform"
)

func offsiteFetchUploads(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (the host copy)")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	uploadsDir := fs.String("uploads-dir", "", "host directory of the submission uploads volume (default: resolved from the uploads PVC, binding it if needed)")
	uploadsPVC := fs.String("uploads-pvc", platform.UploadsPVCName, "the submission uploads PVC, in the control-plane namespace")
	at := fs.String("at", "", "uploads index version to restore (default: the newest; `felis offsite list` shows them)")
	adopt := fs.Bool("adopt-volume", false, "on a host with no S3 uploads store set up, keep uploads on the uploads volume, as the host that wrote the copies did (felis restore-host passes it)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-uploads: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	stamp, idx, err := offsite.ChooseUploadIndex(ctx, env.bucket, env.key, *at)
	if errors.Is(err, offsite.ErrNoUploadIndex) && *at == "" {
		fmt.Fprintln(stdout, "felis offsite fetch-uploads: the bucket holds no uploads, so there is nothing to put back")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-uploads: %v\n", err)
		return 1
	}
	dir := *uploadsDir
	if dir == "" && *adopt && adoptsUploadsVolume(cfg.Registry) {
		fmt.Fprintln(stdout, "felis offsite fetch-uploads: this host has no uploads store set up, so it now keeps uploads on its uploads volume, as the host that wrote the copies did")
		if err := applyStorageConfig(ctx, storageLocal, s3Inputs{}); err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-uploads: switch uploads to the volume: %v\n", err)
			return 1
		}
		cfg.Registry.UserUploadsContext = platform.UploadsLocalPath
	}
	if dir == "" {
		if !isLocalUploadsPath(cfg.Registry.UserUploadsContext) {
			fmt.Fprintf(stderr, "felis offsite fetch-uploads: [registry] user_uploads_context %q is not the uploads volume; pass -uploads-dir to restore into a directory anyway\n", cfg.Registry.UserUploadsContext)
			return 2
		}
		if dir, err = resolveVolumeDir(ctx, platform.DefaultControlNamespace, *uploadsPVC, uploadsVolume, true, stderr); err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-uploads: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "felis offsite fetch-uploads: restoring uploads index %s (%d submission contexts, %s) into %s\n",
		stamp, len(idx.Contexts), offsite.HumanBytes(idx.Bytes()), dir)
	res, err := offsite.FetchUploads(ctx, env.bucket, env.key, idx, dir, platform.ControlPlaneUID, platform.ControlPlaneUID, stderr)
	fmt.Fprintf(stdout, "felis offsite fetch-uploads: %d written (%s), %d already in place, %d failed\n",
		res.Written, offsite.HumanBytes(res.Bytes), res.Present, len(res.Failures))
	for _, f := range res.Failures {
		fmt.Fprintf(stderr, "felis offsite fetch-uploads: %s\n", f)
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-uploads: %v (a second run writes only what is still missing)\n", err)
		return 1
	}
	return 0
}

// adoptsUploadsVolume reports whether uploads have nowhere to go but the volume: no S3 store is set up, only the placeholder.
func adoptsUploadsVolume(reg config.RegistryConfig) bool {
	return !isLocalUploadsPath(reg.UserUploadsContext) && reg.S3 == (config.RegistryS3Config{})
}
