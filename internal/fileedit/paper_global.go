package fileedit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const MaxPaperGlobalPatchBytes = 64 << 10

type PaperGlobalConfig struct {
	Values map[string]any `json:"values"`
	SHA256 string         `json:"sha256"`
}

func (c *PaperGlobalConfig) UnmarshalJSON(raw []byte) error {
	type plain PaperGlobalConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode((*plain)(c))
}

type paperGlobalKind uint8

const (
	paperGlobalBool paperGlobalKind = iota
	paperGlobalInt
	paperGlobalNumber
	paperGlobalString
)

type paperGlobalField struct {
	path []string
	kind paperGlobalKind
	min  float64
}

var paperGlobalFields = map[string]paperGlobalField{
	"chunk-loading-advanced.auto-config-send-distance":             {path: []string{"chunk-loading-advanced", "auto-config-send-distance"}, kind: paperGlobalBool},
	"collisions.enable-player-collisions":                          {path: []string{"collisions", "enable-player-collisions"}, kind: paperGlobalBool},
	"collisions.send-full-pos-for-hard-colliding-entities":         {path: []string{"collisions", "send-full-pos-for-hard-colliding-entities"}, kind: paperGlobalBool},
	"commands.suggest-player-names-when-null-tab-completions":      {path: []string{"commands", "suggest-player-names-when-null-tab-completions"}, kind: paperGlobalBool},
	"console.enable-brigadier-completions":                         {path: []string{"console", "enable-brigadier-completions"}, kind: paperGlobalBool},
	"console.enable-brigadier-highlighting":                        {path: []string{"console", "enable-brigadier-highlighting"}, kind: paperGlobalBool},
	"messages.use-display-name-in-quit-message":                    {path: []string{"messages", "use-display-name-in-quit-message"}, kind: paperGlobalBool},
	"scoreboards.save-empty-scoreboard-teams":                      {path: []string{"scoreboards", "save-empty-scoreboard-teams"}, kind: paperGlobalBool},
	"scoreboards.track-plugin-scoreboards":                         {path: []string{"scoreboards", "track-plugin-scoreboards"}, kind: paperGlobalBool},
	"chunk-loading-advanced.player-max-concurrent-chunk-generates": {path: []string{"chunk-loading-advanced", "player-max-concurrent-chunk-generates"}, kind: paperGlobalInt, min: -1},
	"chunk-loading-advanced.player-max-concurrent-chunk-loads":     {path: []string{"chunk-loading-advanced", "player-max-concurrent-chunk-loads"}, kind: paperGlobalInt, min: -1},
	"player-auto-save.max-per-tick":                                {path: []string{"player-auto-save", "max-per-tick"}, kind: paperGlobalInt, min: -1},
	"player-auto-save.rate":                                        {path: []string{"player-auto-save", "rate"}, kind: paperGlobalInt, min: -1},
	"watchdog.early-warning-delay":                                 {path: []string{"watchdog", "early-warning-delay"}, kind: paperGlobalInt},
	"watchdog.early-warning-every":                                 {path: []string{"watchdog", "early-warning-every"}, kind: paperGlobalInt},
	"chunk-loading-basic.player-max-chunk-generate-rate":           {path: []string{"chunk-loading-basic", "player-max-chunk-generate-rate"}, kind: paperGlobalNumber},
	"chunk-loading-basic.player-max-chunk-load-rate":               {path: []string{"chunk-loading-basic", "player-max-chunk-load-rate"}, kind: paperGlobalNumber},
	"chunk-loading-basic.player-max-chunk-send-rate":               {path: []string{"chunk-loading-basic", "player-max-chunk-send-rate"}, kind: paperGlobalNumber},
	"messages.no-permission":                                       {path: []string{"messages", "no-permission"}, kind: paperGlobalString},
	"messages.kick.connection-throttle":                            {path: []string{"messages", "kick", "connection-throttle"}, kind: paperGlobalString},
}

func paperGlobalNumeric(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case int:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint64:
		number = float64(value)
	case float64:
		number = value
	case json.Number:
		var err error
		number, err = value.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func (f paperGlobalField) accepts(value any) bool {
	switch f.kind {
	case paperGlobalBool:
		_, ok := value.(bool)
		return ok
	case paperGlobalString:
		text, ok := value.(string)
		return ok && utf8.ValidString(text)
	case paperGlobalInt, paperGlobalNumber:
		number, ok := paperGlobalNumeric(value)
		return ok && (f.kind != paperGlobalInt || math.Trunc(number) == number)
	}
	return false
}

func ValidatePaperGlobalPatch(values map[string]any) error {
	for id, value := range values {
		field, ok := paperGlobalFields[id]
		if !ok || !field.accepts(value) {
			return ErrBadPath
		}
		switch field.kind {
		case paperGlobalInt:
			number, _ := paperGlobalNumeric(value)
			if number < field.min || number > math.MaxInt32 {
				return ErrBadPath
			}
		case paperGlobalNumber:
			number, _ := paperGlobalNumeric(value)
			if number != -1 && number < 0 {
				return ErrBadPath
			}
		case paperGlobalString:
			text := value.(string)
			if len(text) > 4096 || strings.ContainsRune(text, 0) {
				return ErrBadPath
			}
		}
	}
	return nil
}

func paperGlobalSHA(sum string) bool {
	return len(sum) == 64 && strings.Trim(sum, "0123456789abcdef") == ""
}

func safePaperGlobalError(err error) error {
	for _, known := range []error{ErrBadPath, ErrNotFound, ErrTooLarge, ErrConflict, ErrNoSpace, ErrReadDamaged, ErrDigestMismatch, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, known) {
			return known
		}
	}
	return errors.New("fileedit: protected configuration operation failed")
}

func (e *Editor) ReadPaperGlobal(ctx context.Context, server string) (PaperGlobalConfig, error) {
	return e.runPaperGlobal(ctx, server, JobParams{Op: OpPaperGlobalRead})
}

func (e *Editor) PatchPaperGlobal(ctx context.Context, server string, values map[string]any, expect string) (PaperGlobalConfig, error) {
	if !paperGlobalSHA(expect) || ValidatePaperGlobalPatch(values) != nil {
		return PaperGlobalConfig{}, ErrBadPath
	}
	if values == nil {
		values = map[string]any{}
	}
	content, err := json.Marshal(values)
	if err != nil {
		return PaperGlobalConfig{}, ErrBadPath
	}
	if len(content) > MaxPaperGlobalPatchBytes {
		return PaperGlobalConfig{}, ErrTooLarge
	}
	return e.runPaperGlobal(ctx, server, JobParams{Op: OpPaperGlobalPatch, Content: content, Expect: expect})
}

func (e *Editor) runPaperGlobal(ctx context.Context, server string, params JobParams) (PaperGlobalConfig, error) {
	res, err := e.run(ctx, server, params)
	if err != nil {
		return PaperGlobalConfig{}, safePaperGlobalError(err)
	}
	if res.PaperGlobal == nil || !paperGlobalSHA(res.PaperGlobal.SHA256) {
		return PaperGlobalConfig{}, ErrReadDamaged
	}
	for id, value := range res.PaperGlobal.Values {
		field, ok := paperGlobalFields[id]
		if !ok || !field.accepts(value) {
			return PaperGlobalConfig{}, ErrReadDamaged
		}
	}
	if res.PaperGlobal.Values == nil {
		res.PaperGlobal.Values = map[string]any{}
	}
	return *res.PaperGlobal, nil
}

func paperGlobalFailure(err error) Result {
	code := CodeBadPath
	if errors.Is(err, fs.ErrNotExist) {
		code = CodeNotFound
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		code = CodeNoSpace
	}
	return Result{Code: code, Error: "protected configuration is not accessible"}
}

type paperGlobalSource struct {
	root     *os.Root
	dir      *os.Root
	file     *os.File
	dirInfo  fs.FileInfo
	fileInfo fs.FileInfo
	sum      string
}

func openPaperGlobal(r *os.Root) (*paperGlobalSource, []byte, Result) {
	if res := noLinks(r, secretConfigPath); res.Code != "" {
		res.Error, res.Entry = "protected configuration is not accessible", ""
		return nil, nil, res
	}
	parent, err := r.Lstat("config")
	if err != nil || !parent.IsDir() {
		return nil, nil, paperGlobalFailure(err)
	}
	dir, err := r.OpenRoot("config")
	if err != nil {
		return nil, nil, paperGlobalFailure(err)
	}
	info, err := dir.Lstat("paper-global.yml")
	if err != nil || !info.Mode().IsRegular() {
		dir.Close()
		return nil, nil, paperGlobalFailure(err)
	}
	f, err := dir.OpenFile("paper-global.yml", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		dir.Close()
		return nil, nil, paperGlobalFailure(err)
	}
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		f.Close()
		dir.Close()
		return nil, nil, paperGlobalFailure(err)
	}
	if current.Size() > MaxReadBytes {
		f.Close()
		dir.Close()
		return nil, nil, Result{Code: CodeTooLarge, Error: "protected configuration exceeds the editor limit"}
	}
	content, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil || len(content) > MaxReadBytes {
		f.Close()
		dir.Close()
		if len(content) > MaxReadBytes {
			return nil, nil, Result{Code: CodeTooLarge, Error: "protected configuration exceeds the editor limit"}
		}
		return nil, nil, paperGlobalFailure(err)
	}
	return &paperGlobalSource{root: r, dir: dir, file: f, dirInfo: parent, fileInfo: current, sum: digest(content)}, content, Result{}
}

func (s *paperGlobalSource) sameIdentity() bool {
	if res := noLinks(s.root, secretConfigPath); res.Code != "" {
		return false
	}
	parent, err := s.root.Lstat("config")
	if err != nil || !os.SameFile(s.dirInfo, parent) {
		return false
	}
	pinned, err := s.dir.Stat(".")
	if err != nil || !os.SameFile(s.dirInfo, pinned) {
		return false
	}
	current, err := s.file.Stat()
	if err != nil || !os.SameFile(s.fileInfo, current) || current.Size() > MaxReadBytes {
		return false
	}
	before, ok := s.fileInfo.Sys().(*syscall.Stat_t)
	after, currentOK := current.Sys().(*syscall.Stat_t)
	if !ok || !currentOK || before.Nlink != after.Nlink {
		return false
	}
	landed, err := s.dir.Lstat("paper-global.yml")
	if err != nil || !landed.Mode().IsRegular() || !os.SameFile(current, landed) {
		return false
	}
	return true
}

func (s *paperGlobalSource) unchanged() Result {
	changed := Result{Code: CodeConflict, Error: "protected configuration changed; reload it before saving"}
	if !s.sameIdentity() {
		return changed
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return changed
	}
	content, err := io.ReadAll(io.LimitReader(s.file, MaxReadBytes+1))
	if err != nil || len(content) > MaxReadBytes || digest(content) != s.sum || !s.sameIdentity() {
		return changed
	}
	return Result{}
}

func paperGlobalDocument(content []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var document, extra yaml.Node
	if decoder.Decode(&document) != nil || decoder.Decode(&extra) != io.EOF || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, ErrBadPath
	}
	var check func(*yaml.Node) bool
	check = func(node *yaml.Node) bool {
		if node.Anchor != "" || node.Kind == yaml.AliasNode || node.Tag == "!!merge" {
			return false
		}
		if node.Kind == yaml.MappingNode {
			keys := map[string]bool{}
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || keys[key.Value] {
					return false
				}
				keys[key.Value] = true
			}
		}
		for _, child := range node.Content {
			if !check(child) {
				return false
			}
		}
		return true
	}
	if !check(&document) {
		return nil, ErrBadPath
	}
	return &document, nil
}

func paperGlobalLeaf(document *yaml.Node, path []string) *yaml.Node {
	node := document.Content[0]
	for _, part := range path {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var found *yaml.Node
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == part {
				found = node.Content[i+1]
				break
			}
		}
		if found == nil {
			return nil
		}
		node = found
	}
	return node
}

func (f paperGlobalField) value(node *yaml.Node) (any, bool) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return nil, false
	}
	tag := map[paperGlobalKind]string{paperGlobalBool: "!!bool", paperGlobalInt: "!!int", paperGlobalString: "!!str"}
	if f.kind == paperGlobalNumber {
		if node.Tag != "!!int" && node.Tag != "!!float" {
			return nil, false
		}
	} else if node.Tag != tag[f.kind] {
		return nil, false
	}
	var value any
	if node.Decode(&value) != nil || !f.accepts(value) {
		return nil, false
	}
	return value, true
}

func paperGlobalValues(document *yaml.Node) map[string]any {
	values := map[string]any{}
	for id, field := range paperGlobalFields {
		if value, ok := field.value(paperGlobalLeaf(document, field.path)); ok {
			values[id] = value
		}
	}
	return values
}

func (f paperGlobalField) update(node *yaml.Node, value any) bool {
	old, _ := f.value(node)
	switch f.kind {
	case paperGlobalBool:
		if old == value {
			return false
		}
		node.Value, node.Tag = strconv.FormatBool(value.(bool)), "!!bool"
	case paperGlobalString:
		if old == value {
			return false
		}
		node.Value, node.Tag = value.(string), "!!str"
	case paperGlobalInt, paperGlobalNumber:
		before, _ := paperGlobalNumeric(old)
		after, _ := paperGlobalNumeric(value)
		if before == after {
			return false
		}
		if f.kind == paperGlobalInt {
			node.Value, node.Tag = strconv.FormatInt(int64(after), 10), "!!int"
		} else {
			node.Value, node.Tag = strconv.FormatFloat(after, 'g', -1, 64), "!!float"
		}
	}
	return true
}

func paperGlobal(r *os.Root, req Request) Result {
	if req.Path != "" || req.To != "" {
		return Result{Code: CodeBadPath, Error: "protected configuration uses a fixed path"}
	}
	var patch map[string]any
	if req.Op == OpPaperGlobalPatch {
		if len(req.Content) > MaxPaperGlobalPatchBytes {
			return Result{Code: CodeTooLarge, Error: "protected configuration patch exceeds the request limit"}
		}
		if req.ContentSHA256 == "" || req.ContentSHA256 != digest(req.Content) {
			return Result{Code: CodeDigestMismatch, Error: "protected configuration patch was damaged in transit"}
		}
		decoder := json.NewDecoder(bytes.NewReader(req.Content))
		decoder.UseNumber()
		var extra any
		if !paperGlobalSHA(req.Expect) || decoder.Decode(&patch) != nil || patch == nil || decoder.Decode(&extra) != io.EOF || ValidatePaperGlobalPatch(patch) != nil {
			return Result{Code: CodeBadPath, Error: "invalid protected configuration patch"}
		}
	}
	source, content, res := openPaperGlobal(r)
	if res.Code != "" {
		return res
	}
	defer source.dir.Close()
	defer source.file.Close()
	if req.Op == OpPaperGlobalPatch {
		if source.sum != req.Expect {
			return Result{Code: CodeConflict, Error: "protected configuration changed; reload it before saving"}
		}
		if stat, ok := source.fileInfo.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
			return Result{Code: CodeBadPath, Error: "protected configuration cannot be patched through hard-link aliases"}
		}
	}
	document, err := paperGlobalDocument(content)
	if err != nil {
		return Result{Code: CodeBadPath, Error: "protected configuration has an unsupported YAML structure"}
	}
	changed := false
	for id, value := range patch {
		field := paperGlobalFields[id]
		node := paperGlobalLeaf(document, field.path)
		if _, ok := field.value(node); !ok {
			return Result{Code: CodeBadPath, Error: "the requested setting is absent or has an incompatible type"}
		}
		changed = field.update(node, value) || changed
	}
	if !changed {
		if res := source.unchanged(); res.Code != "" {
			return res
		}
		return Result{PaperGlobal: &PaperGlobalConfig{Values: paperGlobalValues(document), SHA256: source.sum}}
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if encoder.Encode(document) != nil || encoder.Close() != nil {
		return Result{Code: CodeBadPath, Error: "protected configuration could not be encoded"}
	}
	if encoded.Len() > MaxWriteBytes {
		return Result{Code: CodeTooLarge, Error: "protected configuration would exceed the editor limit"}
	}
	res, err = landChecked(source.dir, secretConfigPath, "paper-global.yml", 0600, func(w io.Writer) error {
		_, err := w.Write(encoded.Bytes())
		return err
	}, source.unchanged)
	if err != nil {
		return paperGlobalFailure(err)
	}
	if res.Code != "" {
		res.Error, res.Entry = "protected configuration could not be saved", ""
		return res
	}
	return Result{PaperGlobal: &PaperGlobalConfig{Values: paperGlobalValues(document), SHA256: digest(encoded.Bytes())}}
}
