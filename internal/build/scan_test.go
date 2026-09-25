package build

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// trivyFixture is a trimmed Trivy 0.74 JSON report: an OS layer and a jar layer,
// with fixed, unfixed and status-only-fixed vulnerabilities, a leaked secret and a
// severity Trivy never assigns.
const trivyFixture = `{
  "SchemaVersion": 2,
  "ArtifactName": "/image/image.tar",
  "Results": [
    {
      "Target": "image.tar (ubuntu 24.04)",
      "Class": "os-pkgs",
      "Packages": [{"Name": "libc6"}, {"Name": "openssl"}, {"Name": "zlib1g"}],
      "Vulnerabilities": [
        {"VulnerabilityID": "CVE-2024-0003", "PkgName": "zlib1g", "InstalledVersion": "1.3", "FixedVersion": "1.3.1", "Status": "fixed", "Severity": "MEDIUM", "Title": "zlib overflow"},
        {"VulnerabilityID": "CVE-2024-0002", "PkgName": "openssl", "InstalledVersion": "3.0.13", "FixedVersion": "", "Status": "affected", "Severity": "HIGH", "Title": "openssl timing"},
        {"VulnerabilityID": "CVE-2024-0009", "PkgName": "libc6", "InstalledVersion": "2.39", "Status": "affected", "Severity": "bogus"}
      ]
    },
    {
      "Target": "data/mods/core.jar",
      "Class": "lang-pkgs",
      "Packages": [{"Name": "log4j-core"}, {"Name": "commons-text"}],
      "Vulnerabilities": [
        {"VulnerabilityID": "CVE-2024-0004", "PkgName": "commons-text", "InstalledVersion": "1.9", "Status": "fixed", "Severity": "HIGH"},
        {"VulnerabilityID": "CVE-2024-0001", "PkgName": "log4j-core", "InstalledVersion": "2.14.1", "FixedVersion": "2.17.1", "Status": "fixed", "Severity": "CRITICAL", "Title": "Log4Shell"}
      ]
    },
    {
      "Target": "/data/server.properties",
      "Class": "secret",
      "Secrets": [{"RuleID": "aws-access-key-id", "Category": "AWS", "Severity": "CRITICAL", "Title": "AWS Access Key ID"}]
    }
  ]
}`

func findingIDs(fs []ScanFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s:%t", f.ID, f.Blocking))
	}
	return out
}

func TestParseSeverities(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		err  string
	}{
		{in: "high, critical", want: []string{"CRITICAL", "HIGH"}},
		{in: "LOW,medium,MEDIUM", want: []string{"MEDIUM", "LOW"}},
		{in: "unknown", want: []string{"UNKNOWN"}},
		{in: "HIGH,SEVERE", err: `unknown severity "SEVERE" (use CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN)`},
		{in: " , ", err: "no severity named"},
	} {
		got, err := ParseSeverities(tc.in)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("ParseSeverities(%q) err = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseSeverities(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseScanAccept(t *testing.T) {
	got, err := ParseScanAccept(" CVE-2021-35515,GHSA-cfgp-2977-2fmm, ,aws-access-key-id,CVE-2021-35515,DLA-3782-1,RHSA-2024:1234")
	if err != nil || !reflect.DeepEqual(got, []string{"CVE-2021-35515", "GHSA-cfgp-2977-2fmm", "aws-access-key-id", "DLA-3782-1", "RHSA-2024:1234"}) {
		t.Errorf("ParseScanAccept = %v, %v", got, err)
	}
	if got, err := ParseScanAccept(""); err != nil || got != nil {
		t.Errorf("empty = %v, %v; want nothing accepted", got, err)
	}
	for in, want := range map[string]string{
		"CVE-2021-35515 CVE-2025-67030": `"CVE-2021-35515 CVE-2025-67030" is not a vulnerability id or secret rule id`,
		"-rf":                           `"-rf" is not a vulnerability id or secret rule id`,
		strings.Repeat("A", 129):        `"` + strings.Repeat("A", 129) + `" is not a vulnerability id or secret rule id`,
	} {
		if _, err := ParseScanAccept(in); err == nil || err.Error() != want {
			t.Errorf("ParseScanAccept(%.20q) err = %v", in, err)
		}
	}
	if got, err := ParseScanAccept(strings.Repeat("A", 128)); err != nil || len(got) != 1 {
		t.Errorf("a 128-character id = %v, %v", got, err)
	}
}

// An accepted id stays counted and listed, marked accepted, and never blocks;
// the other findings are judged as before.
func TestSummarizeAcceptedIDsNeverBlock(t *testing.T) {
	s, err := Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"CRITICAL", "HIGH"},
		Accept: []string{"CVE-2024-0001", "aws-access-key-id", "CVE-2099-0001"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	var got []string
	for _, f := range s.Findings {
		got = append(got, fmt.Sprintf("%s:%t:%t", f.ID, f.Blocking, f.Accepted))
	}
	want := []string{"CVE-2024-0004:true:false", "CVE-2024-0001:false:true", "aws-access-key-id:false:true",
		"CVE-2024-0002:false:false", "CVE-2024-0003:false:false", "CVE-2024-0009:false:false"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(s.Counts, map[string]int{"CRITICAL": 2, "HIGH": 2, "MEDIUM": 1, "UNKNOWN": 1}) ||
		!reflect.DeepEqual(s.BlockingCounts, map[string]int{"HIGH": 1}) {
		t.Errorf("counts = %v, blocking = %v", s.Counts, s.BlockingCounts)
	}
	if got := s.Reason(); got != "the scan blocked the image: 1 HIGH (CVE-2024-0004)" {
		t.Errorf("reason = %q", got)
	}
	raw, err := json.Marshal(s.Policy)
	if err != nil || string(raw) != `{"fail_on":["CRITICAL","HIGH"],"fail_unfixed":false,"accept":["CVE-2024-0001","aws-access-key-id","CVE-2099-0001"]}` {
		t.Errorf("policy json = %s, %v", raw, err)
	}
}

// A CRITICAL,HIGH policy blocks HIGH and CRITICAL findings that have a fixed release
// (a FixedVersion, or Trivy's status "fixed"), and a leaked secret always.
func TestSummarizeDefaultPolicy(t *testing.T) {
	s, err := Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"CRITICAL", "HIGH"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !s.Blocked || s.Packages != 5 {
		t.Errorf("blocked=%t packages=%d, want true and 5", s.Blocked, s.Packages)
	}
	if want := map[string]int{"CRITICAL": 2, "HIGH": 2, "MEDIUM": 1, "UNKNOWN": 1}; !reflect.DeepEqual(s.Counts, want) {
		t.Errorf("counts = %v, want %v", s.Counts, want)
	}
	if want := map[string]int{"CRITICAL": 2, "HIGH": 1}; !reflect.DeepEqual(s.BlockingCounts, want) {
		t.Errorf("blocking counts = %v, want %v", s.BlockingCounts, want)
	}
	want := []string{"CVE-2024-0001:true", "aws-access-key-id:true", "CVE-2024-0004:true",
		"CVE-2024-0002:false", "CVE-2024-0003:false", "CVE-2024-0009:false"}
	if got := findingIDs(s.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %v\nwant %v", got, want)
	}
	first := s.Findings[0]
	if first != (ScanFinding{ID: "CVE-2024-0001", Kind: "vulnerability", Severity: "CRITICAL", Package: "log4j-core",
		Installed: "2.14.1", Fixed: "2.17.1", Target: "data/mods/core.jar", Title: "Log4Shell", Blocking: true}) {
		t.Errorf("first finding = %+v", first)
	}
	if sec := s.Findings[1]; sec.Kind != "secret" || sec.Target != "/data/server.properties" || sec.Title != "AWS Access Key ID" {
		t.Errorf("secret finding = %+v", sec)
	}
	if got := s.Reason(); got != "the scan blocked the image: 2 CRITICAL, 1 HIGH (CVE-2024-0001, aws-access-key-id, CVE-2024-0004)" {
		t.Errorf("reason = %q", got)
	}
}

func TestSummarizePolicyVariants(t *testing.T) {
	s, err := Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"CRITICAL", "HIGH"}, FailUnfixed: true})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := map[string]int{"CRITICAL": 2, "HIGH": 2}; !reflect.DeepEqual(s.BlockingCounts, want) {
		t.Errorf("fail-unfixed blocking counts = %v, want %v", s.BlockingCounts, want)
	}

	s, err = Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"CRITICAL"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got := s.Reason(); got != "the scan blocked the image: 2 CRITICAL (CVE-2024-0001, aws-access-key-id)" {
		t.Errorf("CRITICAL-only reason = %q", got)
	}

	s, err = Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"LOW"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Blocked || s.Reason() != "" || len(s.BlockingCounts) != 0 {
		t.Errorf("LOW-only policy blocked=%t reason=%q counts=%v, want nothing blocking", s.Blocked, s.Reason(), s.BlockingCounts)
	}
}

func TestSummarizeCleanAndBadReports(t *testing.T) {
	s, err := Summarize([]byte(`{"SchemaVersion":2,"Results":[{"Target":"x","Packages":[{}]}]}`), ScanPolicy{FailOn: DefaultScanFailOn})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Blocked || s.Packages != 1 || s.Findings == nil || len(s.Findings) != 0 {
		t.Errorf("clean report = %+v, want unblocked, one package, an empty findings list", s)
	}
	if _, err := Summarize([]byte(`{"SchemaVersion":1,"Results":[]}`), ScanPolicy{}); err == nil ||
		err.Error() != "read trivy report: schema version 1, want 2" {
		t.Errorf("schema 1 err = %v", err)
	}
	if _, err := Summarize([]byte(`not json`), ScanPolicy{}); err == nil || !strings.HasPrefix(err.Error(), "read trivy report: ") {
		t.Errorf("garbage err = %v", err)
	}
}

// One CVE in two packages is two blocking findings under one id: the reason
// names the id once and counts the other.
func TestReasonNamesARepeatedIDOnce(t *testing.T) {
	report := `{"SchemaVersion":2,"Results":[{"Target":"mods","Vulnerabilities":[
	 {"VulnerabilityID":"CVE-2024-0001","PkgName":"log4j-core","InstalledVersion":"2.14.1","FixedVersion":"2.17.1","Severity":"CRITICAL"},
	 {"VulnerabilityID":"CVE-2024-0001","PkgName":"log4j-api","InstalledVersion":"2.14.1","FixedVersion":"2.17.1","Severity":"CRITICAL"}]}]}`
	s, err := Summarize([]byte(report), ScanPolicy{FailOn: DefaultScanFailOn})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Reason(); got != "the scan blocked the image: 2 CRITICAL (CVE-2024-0001 and 1 more)" {
		t.Errorf("reason = %q", got)
	}
}

// A summary lists at most MaxSummaryFindings findings, while the counts and the
// reason still cover every one.
func TestSummarizeCapsTheListedFindings(t *testing.T) {
	var vulns []string
	for i := range 150 {
		vulns = append(vulns, fmt.Sprintf(`{"VulnerabilityID":"CVE-2025-%04d","PkgName":"p","FixedVersion":"2","Severity":"HIGH"}`, i))
	}
	rep := `{"SchemaVersion":2,"Results":[{"Target":"t","Vulnerabilities":[` + strings.Join(vulns, ",") + `]}]}`
	s, err := Summarize([]byte(rep), ScanPolicy{FailOn: []string{"CRITICAL", "HIGH"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(s.Findings) != 100 || s.Counts["HIGH"] != 150 || s.BlockingCounts["HIGH"] != 150 {
		t.Errorf("listed %d, counted %d/%d; want 100 listed of 150", len(s.Findings), s.Counts["HIGH"], s.BlockingCounts["HIGH"])
	}
	if got := s.Reason(); got != "the scan blocked the image: 150 HIGH (CVE-2025-0000, CVE-2025-0001, CVE-2025-0002, CVE-2025-0003, CVE-2025-0004 and 145 more)" {
		t.Errorf("reason = %q", got)
	}
}

func TestScanEnvelopeRoundTripsThroughALog(t *testing.T) {
	summary, err := Summarize([]byte(trivyFixture), ScanPolicy{FailOn: DefaultScanFailOn})
	if err != nil {
		t.Fatal(err)
	}
	sbom := json.RawMessage(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"name":"log4j-core","version":"2.14.1"}]}`)
	var log bytes.Buffer
	log.WriteString("felis scan-gate: 5 packages; findings: CRITICAL 2\n")
	written, err := WriteScanEnvelope(&log, ScanEnvelope{Summary: summary, Report: json.RawMessage(trivyFixture), SBOM: sbom})
	if err != nil {
		t.Fatalf("WriteScanEnvelope: %v", err)
	}
	if written.Summary.Omitted != nil {
		t.Errorf("a small envelope omitted %v", written.Summary.Omitted)
	}
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if len(line) > 100 {
			t.Errorf("log line of %d bytes: the frame must stay line-wrapped", len(line))
		}
	}
	env, err := ReadScanEnvelope(strings.NewReader(log.String() + "trailing line\n"))
	if err != nil {
		t.Fatalf("ReadScanEnvelope: %v", err)
	}
	if !reflect.DeepEqual(env.Summary, summary) {
		t.Errorf("summary came back as %+v", env.Summary)
	}
	var want, got bytes.Buffer
	_ = json.Compact(&want, []byte(trivyFixture))
	_ = json.Compact(&got, env.Report)
	if got.String() != want.String() {
		t.Errorf("report came back as %s", got.String())
	}
	if string(env.SBOM) != string(sbom) {
		t.Errorf("sbom came back as %s", env.SBOM)
	}
}

// The reader takes the last intact frame: scan-gate prints its envelope last, and
// a frame whose checksum does not match is ignored.
func TestReadScanEnvelopeTakesTheLastIntactFrame(t *testing.T) {
	frame := func(blocked bool) string {
		var b bytes.Buffer
		if _, err := WriteScanEnvelope(&b, ScanEnvelope{Summary: ScanSummary{Blocked: blocked, Findings: []ScanFinding{}}}); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	env, err := ReadScanEnvelope(strings.NewReader(frame(false) + frame(true)))
	if err != nil || !env.Summary.Blocked {
		t.Errorf("two frames: got %+v, %v; want the second (blocked)", env, err)
	}
	broken := strings.Replace(frame(false), "sha256=", "sha256=00", 1)
	env, err = ReadScanEnvelope(strings.NewReader(frame(true) + broken))
	if err != nil || !env.Summary.Blocked {
		t.Errorf("intact then broken: got %+v, %v; want the intact one", env, err)
	}
	if _, err := ReadScanEnvelope(strings.NewReader(broken)); !errors.Is(err, ErrNoScanEnvelope) {
		t.Errorf("broken only: err = %v, want ErrNoScanEnvelope", err)
	}
	unterminated := strings.SplitAfter(frame(true), "\n")
	if _, err := ReadScanEnvelope(strings.NewReader(strings.Join(unterminated[:len(unterminated)-2], ""))); !errors.Is(err, ErrNoScanEnvelope) {
		t.Errorf("unterminated: err = %v, want ErrNoScanEnvelope", err)
	}
	if _, err := ReadScanEnvelope(strings.NewReader("fake logs")); !errors.Is(err, ErrNoScanEnvelope) {
		t.Errorf("no frame: err = %v, want ErrNoScanEnvelope", err)
	}
}

// An envelope past the budget drops the SBOM first, then the report, and says so.
func TestWriteScanEnvelopeDropsWhatDoesNotFit(t *testing.T) {
	defer func(old int) { envelopeBudget = old }(envelopeBudget)
	envelopeBudget = 2048
	noise := func(n int) json.RawMessage {
		// Incompressible enough: a JSON string of pseudo-random hex.
		var b strings.Builder
		b.WriteString(`"`)
		x := uint32(2463534242)
		for range n {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			fmt.Fprintf(&b, "%08x", x)
		}
		b.WriteString(`"`)
		return json.RawMessage(b.String())
	}
	base := ScanSummary{Findings: []ScanFinding{}}
	for _, tc := range []struct {
		name         string
		report, sbom json.RawMessage
		omitted      []string
	}{
		{"sbom too big", json.RawMessage(`{"r":1}`), noise(1000), []string{"sbom"}},
		{"both too big", noise(1000), noise(1000), []string{"sbom", "report"}},
	} {
		var log bytes.Buffer
		written, err := WriteScanEnvelope(&log, ScanEnvelope{Summary: base, Report: tc.report, SBOM: tc.sbom})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		env, err := ReadScanEnvelope(&log)
		if err != nil {
			t.Fatalf("%s: read: %v", tc.name, err)
		}
		if !reflect.DeepEqual(written.Summary.Omitted, tc.omitted) || !reflect.DeepEqual(env.Summary.Omitted, tc.omitted) {
			t.Errorf("%s: omitted %v / read back %v, want %v", tc.name, written.Summary.Omitted, env.Summary.Omitted, tc.omitted)
		}
		if env.SBOM != nil || (len(tc.omitted) == 2) != (env.Report == nil) {
			t.Errorf("%s: read back report=%d sbom=%d bytes", tc.name, len(env.Report), len(env.SBOM))
		}
	}
}

func TestNewScanCompressesTheDocuments(t *testing.T) {
	sc, err := newScan("bld-7", &ScanEnvelope{Summary: ScanSummary{Blocked: true}, Report: json.RawMessage(`{"SchemaVersion":2}`)}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if sc.BuildID != "bld-7" || !sc.Summary.Blocked || !sc.ScannedAt.Equal(testNow) || sc.SBOMGz != nil {
		t.Errorf("scan = %+v", sc)
	}
	zr, err := gzip.NewReader(bytes.NewReader(sc.ReportGz))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if string(raw) != `{"SchemaVersion":2}` {
		t.Errorf("report decompresses to %q", raw)
	}
}

func TestPrintableStripsLineBreaks(t *testing.T) {
	got := Printable("evil\nfelis-scan-envelope v1 begin\r\x1b[31m x\u0085y")
	if got != "evil?felis-scan-envelope v1 begin??[31m?x?y" {
		t.Errorf("Printable = %q", got)
	}
}
