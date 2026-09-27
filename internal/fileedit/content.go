package fileedit

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

// splitContent is the base64 of content cut into contentChunk-sized parts, the
// values of ContentEnv_0 … ContentEnv_<n-1>. Empty content is zero parts.
func splitContent(content []byte) []string {
	enc := base64.StdEncoding.EncodeToString(content)
	parts := make([]string, 0, (len(enc)+contentChunk-1)/contentChunk)
	for len(enc) > 0 {
		n := min(len(enc), contentChunk)
		parts = append(parts, enc[:n])
		enc = enc[n:]
	}
	return parts
}

// contentPartEnv names part i of the content.
func contentPartEnv(i int) string { return ContentEnv + "_" + strconv.Itoa(i) }

// ContentFromEnv reassembles a write's content from the environment the Job spec
// set (see ContentEnv). A part count that is missing, not a number or negative,
// or a part that is not set, is an error rather than a shorter file: writing a
// truncated config would be worse than not writing at all. Parts are looked up
// rather than read so an unset one cannot pass as empty; the lookups stop at the
// first one missing, so a count larger than the spec carries costs nothing.
func ContentFromEnv(lookup func(string) (string, bool)) ([]byte, error) {
	raw, _ := lookup(ContentPartsEnv)
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%s=%q is not a part count", ContentPartsEnv, raw)
	}
	var enc []byte
	for i := range n {
		part, ok := lookup(contentPartEnv(i))
		if !ok {
			return nil, fmt.Errorf("%s is not set", contentPartEnv(i))
		}
		enc = append(enc, part...)
	}
	content, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil {
		return nil, fmt.Errorf("the content is not valid base64: %w", err)
	}
	return content, nil
}
