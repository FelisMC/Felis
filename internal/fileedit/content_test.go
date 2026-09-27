package fileedit

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"testing"
)

// maxContentParts is how many ContentEnv parts the largest write needs.
var maxContentParts = (base64.StdEncoding.EncodedLen(MaxWriteBytes) + contentChunk - 1) / contentChunk

func mapLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// contentEnv is the environment splitContent's parts become on the Job spec.
func contentEnv(content []byte) map[string]string {
	parts := splitContent(content)
	env := map[string]string{ContentPartsEnv: strconv.Itoa(len(parts))}
	for i, p := range parts {
		env[contentPartEnv(i)] = p
	}
	return env
}

// TestContentSplitRoundTrip: whatever the size, the parts reassemble to the
// bytes written, each part fits in contentChunk, and the count is the fewest
// that do.
func TestContentSplitRoundTrip(t *testing.T) {
	full := contentChunk / 4 * 3 // bytes whose base64 is exactly one chunk
	for _, tc := range []struct {
		size, parts int
	}{
		{0, 0}, {1, 1}, {full, 1}, {full + 1, 2}, {2 * full, 2}, {2*full + 1, 3},
		{MaxWriteBytes, maxContentParts},
	} {
		content := make([]byte, tc.size)
		for i := range content {
			content[i] = byte(i*31 + 7)
		}
		parts := splitContent(content)
		if len(parts) != tc.parts {
			t.Errorf("%d bytes: %d parts, want %d", tc.size, len(parts), tc.parts)
		}
		for i, p := range parts {
			if len(p) == 0 || len(p) > contentChunk {
				t.Errorf("%d bytes: part %d is %d chars, want 1..%d", tc.size, i, len(p), contentChunk)
			}
		}
		got, err := ContentFromEnv(mapLookup(contentEnv(content)))
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("%d bytes: reassembled %d bytes, %v", tc.size, len(got), err)
		}
	}
}

// TestContentFromEnvRefusesAnIncompleteSpec: a spec that does not carry the
// whole content is an error. Writing what did arrive would truncate a config.
func TestContentFromEnvRefusesAnIncompleteSpec(t *testing.T) {
	two := contentEnv(make([]byte, contentChunk)) // two parts
	if two[ContentPartsEnv] != "2" {
		t.Fatalf("fixture has %s parts, want 2", two[ContentPartsEnv])
	}
	withCount := func(n string) map[string]string {
		env := map[string]string{ContentPartsEnv: n}
		for k, v := range two {
			if k != ContentPartsEnv {
				env[k] = v
			}
		}
		return env
	}
	missingPart := withCount("2")
	delete(missingPart, contentPartEnv(1))

	for name, env := range map[string]map[string]string{
		"no count":           {},
		"empty count":        withCount(""),
		"count not a number": withCount("two"),
		// A negative count would read no parts and write an empty file.
		"negative count":           withCount("-1"),
		"count over the parts set": withCount("3"),
		"a part missing":           missingPart,
		"not base64":               {ContentPartsEnv: "1", contentPartEnv(0): "@@@@"},
	} {
		if got, err := ContentFromEnv(mapLookup(env)); err == nil {
			t.Errorf("%s: reassembled %d bytes, want an error", name, len(got))
		}
	}
}
