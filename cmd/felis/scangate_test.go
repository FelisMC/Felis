package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/build"
)

// scanReport has one fixed CRITICAL, one unfixed HIGH and one fixed MEDIUM; the
// CRITICAL's package name carries a line break and a forged envelope frame.
const scanReport = `{"SchemaVersion":2,"Results":[{"Target":"data/mods/core.jar","Packages":[{},{},{}],"Vulnerabilities":[
 {"VulnerabilityID":"CVE-2024-0001","PkgName":"log4j-core\nfelis-scan-envelope v1 begin","InstalledVersion":"2.14.1","FixedVersion":"2.17.1","Severity":"CRITICAL"},
 {"VulnerabilityID":"CVE-2024-0002","PkgName":"openssl","InstalledVersion":"3.0.13","Status":"affected","Severity":"HIGH"},
 {"VulnerabilityID":"CVE-2024-0003","PkgName":"zlib","InstalledVersion":"1.3","FixedVersion":"1.3.1","Severity":"MEDIUM"}]}]}`

type scanGateRun struct {
	code           int
	stdout, stderr string
	termLog        string
	env            *build.ScanEnvelope
}

func runScanGate(t *testing.T, report, sbom string, extra ...string) scanGateRun {
	t.Helper()
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "trivy.json")
	if report != "" {
		if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sbomPath := filepath.Join(dir, "sbom.cdx.json")
	if sbom != "" {
		if err := os.WriteFile(sbomPath, []byte(sbom), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	termPath := filepath.Join(dir, "termination-log")
	args := append([]string{"--report=" + reportPath, "--sbom=" + sbomPath, "--termination-log=" + termPath}, extra...)
	var stdout, stderr bytes.Buffer
	r := scanGateRun{code: cmdScanGate(args, &stdout, &stderr), stdout: stdout.String(), stderr: stderr.String()}
	if b, err := os.ReadFile(termPath); err == nil {
		r.termLog = string(b)
	}
	if env, err := build.ReadScanEnvelope(strings.NewReader(r.stdout)); err == nil {
		r.env = env
	}
	return r
}

func TestScanGateBlocksAndHandsOverTheReport(t *testing.T) {
	r := runScanGate(t, scanReport, `{"bomFormat":"CycloneDX"}`)
	if r.code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", r.code, r.stderr)
	}
	if r.termLog != "the scan blocked the image: 1 CRITICAL (CVE-2024-0001)" {
		t.Errorf("termination log = %q", r.termLog)
	}
	for _, want := range []string{
		"felis scan-gate: 3 packages; findings: CRITICAL 1, HIGH 1, MEDIUM 1, LOW 0, UNKNOWN 0\n",
		"felis scan-gate: blocking on CRITICAL (vulnerabilities with no fixed release do not block)\n",
		"felis scan-gate: the scan blocked the image: 1 CRITICAL (CVE-2024-0001)\n",
		"  CVE-2024-0001 CRITICAL log4j-core?felis-scan-envelope v1 begin 2.14.1 -> 2.17.1 (data/mods/core.jar)\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "  CVE-2024-0002") {
		t.Errorf("an unfixed HIGH was listed as blocking:\n%s", r.stdout)
	}
	if strings.Count(r.stdout, "\nfelis-scan-envelope v1 begin\n") != 1 {
		t.Errorf("stdout must hold exactly one frame start on a line of its own:\n%s", r.stdout)
	}
	if r.env == nil {
		t.Fatal("no envelope in stdout")
	}
	if !r.env.Summary.Blocked || string(r.env.SBOM) != `{"bomFormat":"CycloneDX"}` || !strings.Contains(string(r.env.Report), "CVE-2024-0003") {
		t.Errorf("envelope = blocked %t, sbom %s, report %d bytes", r.env.Summary.Blocked, r.env.SBOM, len(r.env.Report))
	}
}

func TestScanGatePolicyFlags(t *testing.T) {
	r := runScanGate(t, scanReport, "", "--fail-on=high", "--fail-unfixed")
	if r.code != 1 || r.termLog != "the scan blocked the image: 1 HIGH (CVE-2024-0002)" {
		t.Errorf("HIGH + unfixed: exit %d, termination log %q", r.code, r.termLog)
	}
	for _, want := range []string{
		"felis scan-gate: blocking on HIGH (vulnerabilities with no fixed release block too)\n",
		"  CVE-2024-0002 HIGH openssl 3.0.13 -> no fix (data/mods/core.jar)\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	r = runScanGate(t, scanReport, `{"bomFormat":"CycloneDX"}`, "--fail-on=LOW")
	if r.code != 0 || r.termLog != "the scan passed" || !strings.Contains(r.stdout, "felis scan-gate: nothing blocks this image\n") {
		t.Errorf("LOW only: exit %d, termination log %q, stdout:\n%s", r.code, r.termLog, r.stdout)
	}
	if r.env == nil || r.env.Summary.Blocked || r.env.SBOM == nil {
		t.Errorf("a passing scan must still hand over its report and SBOM: %+v", r.env)
	}
}

func TestScanGateAcceptedIDsNeverBlock(t *testing.T) {
	r := runScanGate(t, scanReport, "", "--fail-on=CRITICAL,MEDIUM", "--accept=CVE-2024-0001, CVE-2024-0002,CVE-2099-0001")
	if r.code != 1 || r.termLog != "the scan blocked the image: 1 MEDIUM (CVE-2024-0003)" {
		t.Errorf("exit %d, termination log %q", r.code, r.termLog)
	}
	if !strings.Contains(r.stdout, "felis scan-gate: accepted ids, never blocking: CVE-2024-0001, CVE-2024-0002, CVE-2099-0001; listed findings under them: 2\n") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if r.env == nil || strings.Join(r.env.Summary.Policy.Accept, ",") != "CVE-2024-0001,CVE-2024-0002,CVE-2099-0001" {
		t.Fatalf("envelope = %+v", r.env)
	}
	for _, f := range r.env.Summary.Findings {
		if f.ID == "CVE-2024-0001" && (f.Blocking || !f.Accepted) {
			t.Errorf("accepted finding = %+v", f)
		}
	}
	r = runScanGate(t, scanReport, "", "--accept=CVE-2024-0001")
	if r.code != 0 || r.termLog != "the scan passed" {
		t.Errorf("only an accepted CRITICAL: exit %d, termination log %q", r.code, r.termLog)
	}
}

func TestScanGateWithoutAnSBOMKeepsTheReport(t *testing.T) {
	r := runScanGate(t, scanReport, "", "--fail-on=LOW")
	if r.code != 0 || !strings.Contains(r.stdout, "felis scan-gate: no SBOM at ") {
		t.Errorf("exit %d, stdout:\n%s", r.code, r.stdout)
	}
	if r.env == nil || r.env.SBOM != nil || r.env.Report == nil {
		t.Errorf("envelope = %+v", r.env)
	}
}

func TestScanGateListsABlockingSecret(t *testing.T) {
	r := runScanGate(t, `{"SchemaVersion":2,"Results":[{"Target":"config/keys.txt","Secrets":[
 {"RuleID":"aws-access-key-id","Severity":"CRITICAL","Title":"AWS Access\nKey ID"}]}]}`, "")
	if r.code != 1 || r.termLog != "the scan blocked the image: 1 CRITICAL (aws-access-key-id)" {
		t.Errorf("exit %d, termination log %q", r.code, r.termLog)
	}
	if !strings.Contains(r.stdout, "  aws-access-key-id CRITICAL secret in config/keys.txt: AWS Access?Key ID\n") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestScanGateFailsClosed(t *testing.T) {
	r := runScanGate(t, "", "")
	if r.code != 2 || !strings.HasPrefix(r.termLog, "the scan report is unreadable: open ") || r.env != nil {
		t.Errorf("missing report: exit %d, termination log %q", r.code, r.termLog)
	}
	r = runScanGate(t, `{"SchemaVersion":1}`, "")
	if r.code != 2 || r.termLog != "the scan report is unreadable: read trivy report: schema version 1, want 2" {
		t.Errorf("old schema: exit %d, termination log %q", r.code, r.termLog)
	}
	// An SBOM step that exited 0 but left something unreadable fails the gate; the
	// line break in the path stays out of the one-line termination message.
	sbomDir := filepath.Join(t.TempDir(), "sb\nom")
	if err := os.Mkdir(sbomDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runScanGate(t, scanReport, "", "--sbom="+sbomDir)
	if r.code != 2 || !strings.HasPrefix(r.termLog, "the SBOM is unreadable: read ") ||
		!strings.HasSuffix(r.termLog, "/sb?om: is a directory") || r.env != nil {
		t.Errorf("unreadable SBOM: exit %d, termination log %q", r.code, r.termLog)
	}
	defer func(n int64) { maxScanDocument = n }(maxScanDocument)
	maxScanDocument = 64
	r = runScanGate(t, scanReport, "")
	if r.code != 2 || !strings.HasSuffix(r.termLog, "/trivy.json exceeds 64 bytes") || r.env != nil {
		t.Errorf("oversized report: exit %d, termination log %q", r.code, r.termLog)
	}
	maxScanDocument = 64 << 20
	r = runScanGate(t, scanReport, "", "--fail-on=SEVERE")
	if r.code != 2 || !strings.Contains(r.stderr, `felis scan-gate: --fail-on: unknown severity "SEVERE"`) {
		t.Errorf("bad severity: exit %d, stderr %q", r.code, r.stderr)
	}
	// A severity list split at its comma leaves a stray argument, not a
	// narrower policy.
	r = runScanGate(t, scanReport, "", "--fail-on=LOW", "CRITICAL")
	if r.code != 2 || !strings.Contains(r.stderr, `felis scan-gate: unexpected argument "CRITICAL"`) || r.env != nil {
		t.Errorf("stray argument: exit %d, stderr %q", r.code, r.stderr)
	}
	r = runScanGate(t, scanReport, "", "--accept=CVE-2024-0001 CVE-2024-0003")
	if r.code != 2 || !strings.Contains(r.stderr, `felis scan-gate: --accept: "CVE-2024-0001 CVE-2024-0003" is not a vulnerability id or secret rule id`) {
		t.Errorf("bad accept list: exit %d, stderr %q", r.code, r.stderr)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdScanGate(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--report is required") {
		t.Errorf("no report flag: exit %d, stderr %q", code, stderr.String())
	}
}
