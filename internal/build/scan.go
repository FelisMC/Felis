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

// The scan gate. Trivy scans the built tarball with no severity filter and writes
// its full JSON report (every package, every finding); `trivy convert` turns that
// report into a CycloneDX SBOM; then `felis scan-gate` applies the ScanPolicy,
// prints a readable verdict, and appends the verdict, the report and the SBOM to
// its own log inside a framed, checksummed envelope. felis-api already reads
// build pod logs, so when the Job finishes Sync reads that envelope back and keeps
// all three on the build: a blocked build says which findings blocked it, and an
// admitted image has its report and SBOM on record.

// Severities are the levels Trivy assigns, most severe first.
var Severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// DefaultScanFailOn is the severities that block an image when felis.toml names
// none. HIGH is left to the operator: the platform's own felis/paper image
// carries fixable HIGH findings inside upstream paper.jar (its bundled
// commons-compress and plexus-utils), so blocking on HIGH out of the box fails
// every build FROM it until those ids are listed in scan_accept.
var DefaultScanFailOn = []string{"CRITICAL"}

// ScanPolicy decides which findings block an image.
type ScanPolicy struct {
	// FailOn lists the severities that block, most severe first.
	FailOn []string `json:"fail_on"`
	// FailUnfixed makes a vulnerability with no fixed release block too. Off by
	// default: a submitter cannot upgrade past it, and the report still lists it.
	// A leaked secret always counts as fixable.
	FailUnfixed bool `json:"fail_unfixed"`
	// Accept lists the vulnerability ids and secret rule ids an administrator
	// accepted as known risks: a finding under one of them is still counted and
	// listed, marked accepted, and never blocks.
	Accept []string `json:"accept,omitempty"`
}

// scanIDPattern is the shape of a finding id: CVE-2024-3094, GHSA-…, DLA-…,
// aws-access-key-id. It keeps commas and spaces out of the scan-gate flag.
var scanIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ParseScanAccept reads a comma-separated list of accepted finding ids, in the
// order given, without repeats. An empty list is valid.
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

// ParseSeverities reads a comma-separated severity list ("HIGH,CRITICAL"),
// case-insensitively, into Severities order without repeats.
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
	// Accepted marks a finding whose id the policy accepts; it never blocks.
	Accepted bool `json:"accepted,omitempty"`
}

// Finding kinds.
const (
	FindingVulnerability = "vulnerability"
	FindingSecret        = "secret"
)

// MaxSummaryFindings caps the findings a summary lists. Counts and
// BlockingCounts still cover every finding, and the stored report has them all.
const MaxSummaryFindings = 100

// ScanSummary is the verdict scan-gate reaches on one report.
type ScanSummary struct {
	Policy   ScanPolicy `json:"policy"`
	Blocked  bool       `json:"blocked"`
	Packages int        `json:"packages"`
	// Counts holds every finding by severity; BlockingCounts the ones the policy
	// blocks on.
	Counts         map[string]int `json:"counts"`
	BlockingCounts map[string]int `json:"blocking_counts"`
	// Findings lists blocking findings first, then the rest, most severe first,
	// at most MaxSummaryFindings of them.
	Findings []ScanFinding `json:"findings"`
	// Omitted names the documents ("sbom", "report") left out of the envelope to
	// keep it inside a pod log.
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

// Reason is the one line a blocked build records as its error, naming the
// blocking counts and the first few finding ids. It is empty when nothing blocks.
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

// ScanEnvelope is what scan-gate hands felis-api through its log.
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

// MaxEnvelopeBytes bounds the gzip-compressed envelope. The kubelet rotates a
// container log at 10 MiB by default, and a rotated-away half is gone from
// GetLogs; base64 grows the payload by a third, so 6 MiB keeps the whole frame in
// one file.
const MaxEnvelopeBytes = 6 << 20

// envelopeBudget is MaxEnvelopeBytes, shrunk by tests.
var envelopeBudget = MaxEnvelopeBytes

// maxEnvelopeJSON bounds the decompressed envelope a reader accepts.
const maxEnvelopeJSON = 128 << 20

// ErrNoScanEnvelope means a log holds no complete scan envelope.
var ErrNoScanEnvelope = errors.New("build: no scan envelope in the log")

// WriteScanEnvelope writes env to w as a framed block. When the compressed
// envelope exceeds MaxEnvelopeBytes it drops the SBOM, then the report, and names
// what it dropped in Summary.Omitted. It returns the envelope it wrote.
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

// ReadScanEnvelope returns the last complete, intact envelope in a log. The
// envelope scan-gate writes is the last thing it prints, so a frame that report
// content managed to print earlier can never stand in for it.
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

// Scan is the scan record kept for one build: the verdict, and gzip copies of
// the Trivy report and the CycloneDX SBOM (nil when the envelope left one out).
type Scan struct {
	BuildID   string      `json:"build_id"`
	Summary   ScanSummary `json:"summary"`
	ScannedAt time.Time   `json:"scanned_at"`
	ReportGz  []byte      `json:"-"`
	SBOMGz    []byte      `json:"-"`
}

// newScan compresses an envelope's documents into a Scan.
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

// Printable strips control characters from report text before a log line
// carries it, so a package name from the scanned image cannot start a line of
// its own (such as a forged envelope frame).
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' {
			return '?'
		}
		return r
	}, s)
}
