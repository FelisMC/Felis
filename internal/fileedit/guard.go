package fileedit

import (
	"io/fs"
	"os"
	"path"
)

// What leaves a world mount — a read, a download, a world export, a backup
// export — passes the same two guards: the forwarding-secret file
// (secretConfigPath) is withheld, and server.properties has its RCON password
// redacted (propsPath).
//
// On a live mount both are matched by the file itself (os.SameFile), not by the
// name it was reached under. A plugin runs arbitrary code as the game uid and can
// leave a symbolic or hard link to either file anywhere in the world; a name
// check alone would hand the secret out under the link's name. A stored archive
// has only names, so ArchiveRule matches those.

// Guard knows the two guarded files of one world mount.
type Guard struct {
	secret, props fs.FileInfo
}

// NewGuard looks the guarded files up in r. One that is missing guards nothing:
// no file can be the same file as it.
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

// Rule reports whether the file fi describes must be withheld, or sent only
// through RedactProps.
func (g Guard) Rule(fi fs.FileInfo) (withhold, redact bool) {
	if g.secret != nil && os.SameFile(g.secret, fi) {
		return true, false
	}
	return false, g.props != nil && os.SameFile(g.props, fi)
}

// ArchiveRule is Rule for an entry of a stored world archive, by its name
// cleaned as a path, so "./server.properties" is server.properties too.
func ArchiveRule(name string) (withhold, redact bool) {
	name = path.Clean(name)
	return name == secretConfigPath, name == propsPath
}

// RedactProps replaces the RCON password in server.properties content with
// redactedValue (see redactSecretProps for why a placeholder and not a blank).
func RedactProps(content []byte) []byte {
	return redactSecretProps(propsPath, content)
}
