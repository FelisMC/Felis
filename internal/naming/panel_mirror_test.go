package naming

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The panel's create form checks names as they are typed with its own copy of
// this rule (panel/src/lib/naming.ts). A name reserved or a length changed here
// without the panel would let the form send what the API refuses, or refuse what
// it takes, so the copy is compared to the source.
func TestPanelMirrorsServerNameRule(t *testing.T) {
	raw, err := os.ReadFile("../../panel/src/lib/naming.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	list := regexp.MustCompile(`(?s)RESERVED_SERVER_NAMES: readonly string\[\] = \[(.*?)\];`).FindStringSubmatch(src)
	if list == nil {
		t.Fatal("panel/src/lib/naming.ts: RESERVED_SERVER_NAMES not found")
	}
	var panel []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(list[1], -1) {
		panel = append(panel, m[1])
	}
	var api []string
	for name := range reserved {
		api = append(api, name)
	}
	slices.Sort(panel)
	slices.Sort(api)
	if !slices.Equal(panel, api) {
		t.Errorf("panel reserves %v, API reserves %v", panel, api)
	}

	if want := "const SERVER_NAME_RE = /" + serverNameRE.String() + "/;"; !strings.Contains(src, want) {
		t.Errorf("panel/src/lib/naming.ts lacks %q", want)
	}
}
