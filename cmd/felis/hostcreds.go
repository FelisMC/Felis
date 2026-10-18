package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Store bare host credentials at mode 0600 so installation and backup recovery can recreate Secrets.
const (
	hostSMTPPasswordPath       = "/etc/felis/smtp-password"
	hostUploadsS3AccessKeyPath = "/etc/felis/uploads-s3-access-key"
	hostUploadsS3SecretKeyPath = "/etc/felis/uploads-s3-secret-key"
)

// writeHostCredential replaces the file at path with value, mode 0600, through a
// temporary file in the same directory, so a crash leaves the old value or the
// new one and never a partial one.
func writeHostCredential(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	// CreateTemp already makes the file 0600; the Chmod states it rather than
	// leaning on that.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// readHostCredential returns the value in path; ok is false when there is no
// such file. An empty file is a value: the relay password of a relay without AUTH.
func readHostCredential(path string) (value string, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// Read the host password first, consulting the cluster only when the file is missing.
func relayPassword(ctx context.Context, path string, cl client.Client, ns string) (string, error) {
	if pw, ok, err := readHostCredential(path); err != nil || ok {
		return pw, err
	}
	if cl == nil {
		return "", errClusterUnreachable
	}
	return smtpSecretPassword(ctx, cl, ns)
}

var errClusterUnreachable = errors.New("the cluster did not answer")
