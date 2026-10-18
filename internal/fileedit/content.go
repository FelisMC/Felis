package fileedit

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

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

func contentPartEnv(i int) string { return ContentEnv + "_" + strconv.Itoa(i) }

// ContentFromEnv rejects missing or malformed parts so incomplete content cannot be written as a truncated file.
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
