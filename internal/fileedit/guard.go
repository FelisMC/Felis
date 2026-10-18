package fileedit

import (
	"io/fs"
	"os"
	"path"
)

// Live guards compare file identity to catch symbolic and hard-link aliases; stored archives use cleaned entry names.

type Guard struct {
	secret, props fs.FileInfo
}

// NewGuard ignores absent protected files because no entry can share their file identity.
func NewGuard(r *os.Root) Guard {
	var g Guard
	if fi, err := r.Stat(secretConfigPath); err == nil {
		g.secret = fi
	}
	if fi, err := r.Stat(propsPath); err == nil {
		g.props = fi
	}
	return g
}

func (g Guard) Rule(fi fs.FileInfo) (withhold, redact bool) {
	if g.secret != nil && os.SameFile(g.secret, fi) {
		return true, false
	}
	return false, g.props != nil && os.SameFile(g.props, fi)
}

// ArchiveRule matches cleaned archive paths because stored entries have no live file identity.
func ArchiveRule(name string) (withhold, redact bool) {
	name = path.Clean(name)
	return name == secretConfigPath, name == propsPath
}

func RedactProps(content []byte) []byte {
	return redactSecretProps(propsPath, content)
}
