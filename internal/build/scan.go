package build

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Severities are the levels Trivy assigns, most severe first.
var Severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// HIGH is left to the operator: felis/paper's bundled paper.jar carries fixable HIGH findings,
// so blocking on HIGH would fail every build FROM it until they are in scan_accept.
var DefaultScanFailOn = []string{"CRITICAL"}

// ScanPolicy decides which findings block an image.
type ScanPolicy struct {
	FailOn []string `json:"fail_on"` // blocking severities, most severe first
	// FailUnfixed also blocks vulnerabilities with no fixed release; a leaked secret always counts as fixable.
	FailUnfixed bool `json:"fail_unfixed"`
	// Accept lists vulnerability and secret rule ids that are still reported, marked accepted, but never block.
	Accept []string `json:"accept,omitempty"`
}

// scanIDPattern keeps commas and spaces out of the scan-gate flag.
var scanIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ParseScanAccept reads a comma-separated list of finding ids, in order and without repeats; empty is valid.
func ParseScanAccept(s string) ([]string, error) {
	var out []string
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !scanIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%q is not a vulnerability id or secret rule id", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// ParseSeverities reads a comma-separated severity list case-insensitively into Severities order without repeats.
func ParseSeverities(s string) ([]string, error) {
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !slices.Contains(Severities, part) {
			return nil, fmt.Errorf("unknown severity %q (use %s)", part, strings.Join(Severities, ", "))
		}
		seen[part] = true
	}
	var out []string
	for _, sev := range Severities {
		if seen[sev] {
			out = append(out, sev)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no severity named")
	}
	return out, nil
}

// ScanFinding is one vulnerability or leaked secret in a scan report.
type ScanFinding struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "vulnerability" or "secret"
	Severity  string `json:"severity"`
	Package   string `json:"package,omitempty"`
	Installed string `json:"installed,omitempty"`
	Fixed     string `json:"fixed,omitempty"`
	Target    string `json:"target"`
	Title     string `json:"title,omitempty"`
	Blocking  bool   `json:"blocking"`
	Accepted  bool   `json:"accepted,omitempty"` // the policy accepts this id; it never blocks
}

// Finding kinds.
const (
	FindingVulnerability = "vulnerability"
	FindingSecret        = "secret"
)

// MaxSummaryFindings caps the findings a summary lists; the counts and the stored report cover all of them.
const MaxSummaryFindings = 100

// ScanSummary is the verdict scan-gate reaches on one report.
type ScanSummary struct {
	Policy         ScanPolicy     `json:"policy"`
	Blocked        bool           `json:"blocked"`
	Packages       int            `json:"packages"`
	Counts         map[string]int `json:"counts"`          // every finding by severity
	BlockingCounts map[string]int `json:"blocking_counts"` // findings the policy blocks on
	// Findings lists blocking findings first, then the rest, most severe first, up to MaxSummaryFindings.
	Findings []ScanFinding `json:"findings"`
	// Omitted names the documents ("sbom", "report") dropped to keep the envelope inside a pod log.
	Omitted []string `json:"omitted,omitempty"`
}

// trivyReport is the part of Trivy's JSON report (SchemaVersion 2) the gate reads.
type trivyReport struct {
	SchemaVersion int `json:"SchemaVersion"`
	Results       []struct {
		Target          string     `json:"Target"`
		Packages        []struct{} `json:"Packages"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			Status           string `json:"Status"`
			Severity         string `json:"Severity"`
			Title            string `json:"Title"`
		} `json:"Vulnerabilities"`
		Secrets []struct {
			RuleID   string `json:"RuleID"`
			Severity string `json:"Severity"`
			Title    string `json:"Title"`
		} `json:"Secrets"`
	} `json:"Results"`
}

// Summarize applies policy to a Trivy JSON report.
func Summarize(report []byte, policy ScanPolicy) (ScanSummary, error) {
	var rep trivyReport
	if err := json.Unmarshal(report, &rep); err != nil {
		return ScanSummary{}, fmt.Errorf("read trivy report: %w", err)
	}
	if rep.SchemaVersion != 2 {
		return ScanSummary{}, fmt.Errorf("read trivy report: schema version %d, want 2", rep.SchemaVersion)
	}
	s := ScanSummary{Policy: policy, Counts: map[string]int{}, BlockingCounts: map[string]int{}}
	var all []ScanFinding
	blocks := func(id, sev string, fixable bool) bool {
		return slices.Contains(policy.FailOn, sev) && (fixable || policy.FailUnfixed) && !slices.Contains(policy.Accept, id)
	}
	for _, res := range rep.Results {
		s.Packages += len(res.Packages)
		for _, v := range res.Vulnerabilities {
			sev := severity(v.Severity)
			all = append(all, ScanFinding{
				ID: v.VulnerabilityID, Kind: FindingVulnerability, Severity: sev,
				Package: v.PkgName, Installed: v.InstalledVersion, Fixed: v.FixedVersion,
				Target: res.Target, Title: v.Title,
				Blocking: blocks(v.VulnerabilityID, sev, v.FixedVersion != "" || v.Status == "fixed"),
				Accepted: slices.Contains(policy.Accept, v.VulnerabilityID),
			})
		}
		for _, sec := range res.Secrets {
			sev := severity(sec.Severity)
			all = append(all, ScanFinding{
				ID: sec.RuleID, Kind: FindingSecret, Severity: sev,
				Target: res.Target, Title: sec.Title,
				Blocking: blocks(sec.RuleID, sev, true),
				Accepted: slices.Contains(policy.Accept, sec.RuleID),
			})
		}
	}
	for _, f := range all {
		s.Counts[f.Severity]++
		if f.Blocking {
			s.BlockingCounts[f.Severity]++
			s.Blocked = true
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Blocking != b.Blocking {
			return a.Blocking
		}
		if ra, rb := slices.Index(Severities, a.Severity), slices.Index(Severities, b.Severity); ra != rb {
			return ra < rb
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Package < b.Package
	})
	s.Findings = all[:min(len(all), MaxSummaryFindings)]
	if s.Findings == nil {
		s.Findings = []ScanFinding{}
	}
	return s, nil
}

// severity normalizes a Trivy severity; anything unrecognised is UNKNOWN.
func severity(s string) string {
	s = strings.ToUpper(s)
	if slices.Contains(Severities, s) {
		return s
	}
	return "UNKNOWN"
}

// Reason is the one-line error of a blocked build: the blocking counts and the first few finding ids; empty when nothing blocks.
func (s ScanSummary) Reason() string {
	if !s.Blocked {
		return ""
	}
	var counts []string
	for _, sev := range Severities {
		if n := s.BlockingCounts[sev]; n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	var ids []string
	for _, f := range s.Findings {
		if !f.Blocking || len(ids) == 5 {
			break
		}
		if !slices.Contains(ids, f.ID) {
			ids = append(ids, f.ID)
		}
	}
	total := 0
	for _, n := range s.BlockingCounts {
		total += n
	}
	list := strings.Join(ids, ", ")
	if total > len(ids) {
		list += fmt.Sprintf(" and %d more", total-len(ids))
	}
	return "the scan blocked the image: " + strings.Join(counts, ", ") + " (" + list + ")"
}

// ScanEnvelope is what `felis scan-gate` appends to its log: the verdict, the Trivy report and the CycloneDX SBOM.
type ScanEnvelope struct {
	Summary ScanSummary     `json:"summary"`
	Report  json.RawMessage `json:"report,omitempty"`
	SBOM    json.RawMessage `json:"sbom,omitempty"`
}

const (
	envelopeBegin     = "felis-scan-envelope v1 begin"
	envelopeEndPrefix = "felis-scan-envelope v1 end sha256="
	envelopeLineWidth = 76
)

// MaxEnvelopeBytes bounds the gzip-compressed envelope: the kubelet rotates container logs at 10 MiB by default
// and base64 adds a third, so 6 MiB keeps the whole frame in one file.
const MaxEnvelopeBytes = 6 << 20

// envelopeBudget is MaxEnvelopeBytes, shrunk by tests.
var envelopeBudget = MaxEnvelopeBytes

// maxEnvelopeJSON bounds the decompressed envelope a reader accepts.
const maxEnvelopeJSON = 128 << 20

// ErrNoScanEnvelope means a log holds no complete scan envelope.
var ErrNoScanEnvelope = errors.New("build: no scan envelope in the log")

// Drop SBOM, then report, to fit MaxEnvelopeBytes and record omissions in the returned summary.
func WriteScanEnvelope(w io.Writer, env ScanEnvelope) (ScanEnvelope, error) {
	var payload []byte
	for {
		raw, err := json.Marshal(env)
		if err != nil {
			return env, err
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(raw); err != nil {
			return env, err
		}
		if err := zw.Close(); err != nil {
			return env, err
		}
		payload = buf.Bytes()
		if len(payload) <= envelopeBudget {
			break
		}
		switch {
		case env.SBOM != nil:
			env.SBOM = nil
			env.Summary.Omitted = append(env.Summary.Omitted, "sbom")
		case env.Report != nil:
			env.Report = nil
			env.Summary.Omitted = append(env.Summary.Omitted, "report")
		default:
			return env, fmt.Errorf("build: scan summary alone compresses to %d bytes", len(payload))
		}
	}
	sum := sha256.Sum256(payload)
	enc := base64.StdEncoding.EncodeToString(payload)
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, envelopeBegin)
	for len(enc) > 0 {
		n := min(len(enc), envelopeLineWidth)
		fmt.Fprintln(bw, enc[:n])
		enc = enc[n:]
	}
	fmt.Fprintln(bw, envelopeEndPrefix+hex.EncodeToString(sum[:]))
	return env, bw.Flush()
}

// ReadScanEnvelope returns the last complete, intact envelope in a log, so a frame printed earlier by report content cannot stand in for it.
func ReadScanEnvelope(r io.Reader) (*ScanEnvelope, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var (
		found   []byte
		cur     strings.Builder
		inFrame bool
		budget  = envelopeBudget/3*4 + 4096
	)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == envelopeBegin:
			inFrame = true
			cur.Reset()
		case inFrame && strings.HasPrefix(line, envelopeEndPrefix):
			inFrame = false
			payload, err := base64.StdEncoding.DecodeString(cur.String())
			if err != nil {
				continue
			}
			sum := sha256.Sum256(payload)
			if hex.EncodeToString(sum[:]) == strings.TrimPrefix(line, envelopeEndPrefix) {
				found = payload
			}
		case inFrame:
			if cur.Len()+len(line) > budget {
				inFrame = false
				continue
			}
			cur.WriteString(line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("build: read scan log: %w", err)
	}
	if found == nil {
		return nil, ErrNoScanEnvelope
	}
	zr, err := gzip.NewReader(bytes.NewReader(found))
	if err != nil {
		return nil, fmt.Errorf("build: scan envelope: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxEnvelopeJSON+1))
	if err != nil {
		return nil, fmt.Errorf("build: scan envelope: %w", err)
	}
	if len(raw) > maxEnvelopeJSON {
		return nil, fmt.Errorf("build: scan envelope exceeds %d bytes", maxEnvelopeJSON)
	}
	var env ScanEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("build: scan envelope: %w", err)
	}
	return &env, nil
}

// Scan is the scan record kept for one build; ReportGz and SBOMGz are nil when the envelope left them out.
type Scan struct {
	BuildID   string      `json:"build_id"`
	Summary   ScanSummary `json:"summary"`
	ScannedAt time.Time   `json:"scanned_at"`
	ReportGz  []byte      `json:"-"`
	SBOMGz    []byte      `json:"-"`
}

func newScan(buildID string, env *ScanEnvelope, at time.Time) (Scan, error) {
	s := Scan{BuildID: buildID, Summary: env.Summary, ScannedAt: at}
	var err error
	if s.ReportGz, err = gzipBytes(env.Report); err != nil {
		return s, err
	}
	s.SBOMGz, err = gzipBytes(env.SBOM)
	return s, err
}

func gzipBytes(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Printable replaces control characters in report text so a package name from the scanned image cannot start a log line (such as a forged envelope frame).
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' {
			return '?'
		}
		return r
	}, s)
}
