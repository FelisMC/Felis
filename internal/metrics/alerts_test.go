package metrics

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/yaml"
)

// The shipped alert rules live in deploy/alerts. promtool tests what they do
// (felis-alerts_test.yml); these tests pin what promtool cannot see from there.

type ruleGroups struct {
	Groups []struct {
		Name  string `json:"name"`
		Rules []struct {
			Alert string `json:"alert"`
			Expr  string `json:"expr"`
		} `json:"rules"`
	} `json:"groups"`
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// TestPrometheusRuleMatchesPlainRules: the PrometheusRule twin carries exactly
// the groups of the promtool-tested plain file.
func TestPrometheusRuleMatchesPlainRules(t *testing.T) {
	var plain, twin map[string]any
	readYAML(t, "../../deploy/alerts/felis-alerts.yaml", &plain)
	readYAML(t, "../../deploy/alerts/felis-prometheusrule.yaml", &twin)
	spec, _ := twin["spec"].(map[string]any)
	if !reflect.DeepEqual(plain["groups"], spec["groups"]) {
		t.Fatal("deploy/alerts/felis-prometheusrule.yaml spec.groups differs from felis-alerts.yaml groups; copy the plain file's groups over")
	}
}

// TestAlertRulesUseRealMetrics: every felis_* series a rule reads is one this
// package exports (or the backup timer's textfile series), so a renamed metric
// cannot leave an alert silently watching nothing.
func TestAlertRulesUseRealMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, mf := range families {
		known[mf.GetName()] = true
	}
	// Collectors with no children yet gather nothing; name them from their
	// descriptors instead.
	for _, c := range Collectors() {
		ch := make(chan *prometheus.Desc, 4)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			if m := regexp.MustCompile(`fqName: "([^"]+)"`).FindStringSubmatch(d.String()); m != nil {
				known[m[1]] = true
			}
		}
	}

	var rules ruleGroups
	readYAML(t, "../../deploy/alerts/felis-alerts.yaml", &rules)
	series := regexp.MustCompile(`felis_[a-z_]+`)
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			for _, name := range series.FindAllString(r.Expr, -1) {
				if strings.HasPrefix(name, "felis_db_backup_") {
					continue // written by felis-db-backup.timer for node-exporter
				}
				base := name
				for _, suffix := range []string{"_bucket", "_count", "_sum"} {
					base = strings.TrimSuffix(base, suffix)
				}
				if !known[name] && !known[base] {
					t.Errorf("alert %s reads %s, which no felis collector exports", r.Alert, name)
				}
			}
		}
	}
}
