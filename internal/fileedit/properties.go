package fileedit

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"syscall"
)

func preserveSecretProps(r *os.Root, name, target string, content []byte, expect string) ([]byte, Result) {
	restore := false
	for line := range propertyLines(content) {
		if key, value := secretProperty(line); key != "" && string(value) == redactedValue {
			restore = true
			break
		}
	}
	if !restore {
		return content, Result{}
	}
	info, err := r.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return content, Result{}
	}
	if err != nil {
		return nil, failure(err, name)
	}
	if _, redact := NewGuard(r).Rule(info); !redact {
		return content, Result{}
	}
	f, err := r.Open(target)
	if err != nil {
		return nil, failure(err, name)
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return nil, failure(err, name)
	}
	_, redact := NewGuard(r).Rule(current)
	if !redact || !os.SameFile(info, current) {
		return nil, Result{Code: CodeConflict, Error: "server.properties changed while its redacted values were being preserved"}
	}
	if stat, ok := current.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return nil, Result{Code: CodeBadPath, Error: "redacted credentials can only be preserved when server.properties has no hard-link aliases"}
	}
	if current.Size() > MaxReadBytes {
		return nil, Result{Code: CodeTooLarge, Error: "server.properties is too large to preserve its redacted values"}
	}
	original, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil {
		return nil, failure(err, name)
	}
	if len(original) > MaxReadBytes {
		return nil, Result{Code: CodeTooLarge, Error: "server.properties grew beyond the editor limit"}
	}
	if expect != "" && digest(original) != expect {
		return nil, Result{Code: CodeConflict, SHA256: digest(original), Error: "server.properties changed since it was opened; reload it before saving"}
	}
	values := map[string][]byte{}
	for line := range propertyLines(original) {
		if key, value := secretProperty(line); key != "" {
			values[key] = value
		}
	}
	var restored bytes.Buffer
	restored.Grow(len(content))
	for line := range propertyLines(content) {
		key, value := secretProperty(line)
		stored, exists := values[key]
		if !exists || string(value) != redactedValue {
			restored.Write(line)
			continue
		}
		start := bytes.IndexByte(line, '=') + 1
		ending := line[start+len(value):]
		if restored.Len()+start+len(stored)+len(ending) > MaxWriteBytes {
			return nil, Result{Code: CodeTooLarge, Error: "preserving the redacted values would exceed the editor write limit"}
		}
		restored.Write(line[:start])
		restored.Write(stored)
		restored.Write(ending)
	}
	if restored.Len() > MaxWriteBytes {
		return nil, Result{Code: CodeTooLarge, Error: "preserving the redacted values would exceed the editor write limit"}
	}
	return restored.Bytes(), Result{}
}

func secretProperty(line []byte) (string, []byte) {
	line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	key, value, ok := bytes.Cut(bytes.TrimLeft(line, " \t\f"), []byte("="))
	if !ok || string(key) != rconPasswordKey && string(key) != "forwarding-secrets" {
		return "", nil
	}
	return string(key), value
}

func propertyLines(content []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for remaining := content; len(remaining) > 0; {
			end := bytes.IndexAny(remaining, "\r\n")
			if end < 0 {
				yield(remaining)
				return
			}
			end++
			if remaining[end-1] == '\r' && end < len(remaining) && remaining[end] == '\n' {
				end++
			}
			if !yield(remaining[:end]) {
				return
			}
			remaining = remaining[end:]
		}
	}
}
