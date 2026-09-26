package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Host copies of the credentials `felis setup` takes at the keyboard: the [smtp]
// relay password and the uploads bucket's keys. The cluster reads them from the
// felis-smtp and felis-uploads-s3 Secrets, and a Secret lives in k3s's datastore,
// which a reinstall (uninstall.sh keeps /etc/felis) or a host rebuilt from a
// database bundle's state/ starts empty. Each file holds the bare value, mode
// 0600, directly in /etc/felis beside secrets.env: every installer run applies
// the Secrets from these files, and every database bundle, so the off-site copy
// too, carries them.
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

// relayPassword is the [smtp] relay password as the host holds it: the copy
// `felis setup` keeps at path, else, on an install from before that copy, the
// felis-smtp Secret in ns, whose absence means a relay without AUTH. cl is only
// used when the file is missing; a nil cl then reports errClusterUnreachable.
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
