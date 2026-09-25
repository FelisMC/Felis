package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"felis.lolicon.best/internal/build"
)

// maxScanDocument bounds each document scan-gate reads: a modpack report lists a
// few thousand packages, far below this. Tests shrink it.
var maxScanDocument int64 = 64 << 20

// cmdScanGate is the build pod's verdict step, after trivy wrote its full JSON
// report and trivy convert wrote the CycloneDX SBOM. It applies the scan policy
// to the report, prints the verdict and every blocking finding, then appends the
// verdict, the report and the SBOM to its log as the envelope felis-api keeps on
// the build (build.WriteScanEnvelope). It exits 1 when a finding blocks, which
// fails the pod before the push step runs, and 2 when the report cannot be read,
// so a scan that produced nothing usable never admits an image.
func cmdScanGate(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("scan-gate", flag.ContinueOnError)
	fset.SetOutput(stderr)
	reportPath := fset.String("report", "", "trivy JSON report (required)")
	sbomPath := fset.String("sbom", "", "CycloneDX SBOM to keep with the report")
	failOn := fset.String("fail-on", strings.Join(build.DefaultScanFailOn, ","), "comma-separated severities that block the image")
	failUnfixed := fset.Bool("fail-unfixed", false, "block on vulnerabilities that have no fixed release too")
	accept := fset.String("accept", "", "comma-separated vulnerability ids and secret rule ids that never block")
	termLog := fset.String("termination-log", "/dev/termination-log", "where the one-line verdict goes for the pod status")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	// A stray argument is a policy the gate would otherwise drop without a word
	// (a comma split out of --fail-on, say).
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "felis scan-gate: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	sevs, err := build.ParseSeverities(*failOn)
	if err != nil {
		fmt.Fprintf(stderr, "felis scan-gate: --fail-on: %v\n", err)
		return 2
	}
	accepted, err := build.ParseScanAccept(*accept)
	if err != nil {
		fmt.Fprintf(stderr, "felis scan-gate: --accept: %v\n", err)
		return 2
	}
	if *reportPath == "" {
		fmt.Fprintln(stderr, "felis scan-gate: --report is required")
		return 2
	}
	fail := func(msg string) int {
		fmt.Fprintln(stderr, "felis scan-gate: "+msg)
		writeTerminationLog(*termLog, msg)
		return 2
	}
	report, err := readScanDocument(*reportPath)
	if err != nil {
		return fail("the scan report is unreadable: " + err.Error())
	}
	policy := build.ScanPolicy{FailOn: sevs, FailUnfixed: *failUnfixed, Accept: accepted}
	summary, err := build.Summarize(report, policy)
	if err != nil {
		return fail("the scan report is unreadable: " + err.Error())
	}
	env := build.ScanEnvelope{Summary: summary, Report: report}
	if *sbomPath != "" {
		switch sbom, err := readScanDocument(*sbomPath); {
		case err == nil:
			env.SBOM = sbom
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(stdout, "felis scan-gate: no SBOM at %s; keeping the report alone\n", *sbomPath)
		default:
			return fail("the SBOM is unreadable: " + err.Error())
		}
	}

	printScanVerdict(stdout, summary)
	written, err := build.WriteScanEnvelope(stdout, env)
	if err != nil {
		return fail("could not write the scan envelope: " + err.Error())
	}
	for _, doc := range written.Summary.Omitted {
		fmt.Fprintf(stderr, "felis scan-gate: the %s is too large to keep with the build and was left out\n", doc)
	}
	if summary.Blocked {
		writeTerminationLog(*termLog, summary.Reason())
		return 1
	}
	writeTerminationLog(*termLog, "the scan passed")
	return 0
}

// printScanVerdict writes the human half of scan-gate's log.
func printScanVerdict(w io.Writer, s build.ScanSummary) {
	var counts []string
	for _, sev := range build.Severities {
		counts = append(counts, fmt.Sprintf("%s %d", sev, s.Counts[sev]))
	}
	fmt.Fprintf(w, "felis scan-gate: %d packages; findings: %s\n", s.Packages, strings.Join(counts, ", "))
	unfixed := "vulnerabilities with no fixed release do not block"
	if s.Policy.FailUnfixed {
		unfixed = "vulnerabilities with no fixed release block too"
	}
	fmt.Fprintf(w, "felis scan-gate: blocking on %s (%s)\n", strings.Join(s.Policy.FailOn, ", "), unfixed)
	if len(s.Policy.Accept) > 0 {
		matched := 0
		for _, f := range s.Findings {
			if f.Accepted {
				matched++
			}
		}
		fmt.Fprintf(w, "felis scan-gate: accepted ids, never blocking: %s; listed findings under them: %d\n", strings.Join(s.Policy.Accept, ", "), matched)
	}
	if !s.Blocked {
		fmt.Fprintln(w, "felis scan-gate: nothing blocks this image")
		return
	}
	fmt.Fprintln(w, "felis scan-gate: "+build.Printable(s.Reason()))
	for _, f := range s.Findings {
		if !f.Blocking {
			break
		}
		fix := f.Fixed
		if fix == "" {
			fix = "no fix"
		}
		if f.Kind == build.FindingSecret {
			fmt.Fprintf(w, "  %s %s secret in %s: %s\n", build.Printable(f.ID), f.Severity, build.Printable(f.Target), build.Printable(f.Title))
			continue
		}
		fmt.Fprintf(w, "  %s %s %s %s -> %s (%s)\n", build.Printable(f.ID), f.Severity,
			build.Printable(f.Package), build.Printable(f.Installed), build.Printable(fix), build.Printable(f.Target))
	}
}

func readScanDocument(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxScanDocument+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxScanDocument {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxScanDocument)
	}
	return b, nil
}

// writeTerminationLog leaves msg where the kubelet copies it into the container
// status. It is best effort: the log carries the same verdict.
func writeTerminationLog(path, msg string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(build.Printable(msg)), 0o644)
}
