// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BuildScanPanel } from "./BuildScanPanel";
import type { Build, BuildScan } from "@/lib/types";

const calls = vi.hoisted(() => ({
  getBuildScan: vi.fn(),
  downloadBuildScanDocument: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const FAILED: Build = {
  id: "bld-7",
  image_ref: "registry.felis.svc:5000/user-uploads/s-1:latest",
  status: "failed",
  requested_by: "owner@example.test",
  created_at: "2026-09-20T09:58:00Z",
  finished_at: "2026-09-20T10:00:00Z",
};

// Two blocking findings (a fixable CRITICAL and a leaked key), a fixable HIGH the
// policy accepts, one unfixed HIGH that only gets listed, and two more findings
// past the listed ones.
const BLOCKED: BuildScan = {
  build_id: "bld-7",
  scanned_at: "2026-09-20T10:00:00Z",
  has_report: true,
  has_sbom: false,
  summary: {
    policy: { fail_on: ["CRITICAL", "HIGH"], fail_unfixed: false, accept: ["CVE-2021-35515"] },
    blocked: true,
    packages: 12,
    counts: { CRITICAL: 2, HIGH: 2, MEDIUM: 2 },
    blocking_counts: { CRITICAL: 2 },
    findings: [
      { id: "CVE-2024-0001", kind: "vulnerability", severity: "CRITICAL", package: "log4j-core",
        installed: "2.14.1", fixed: "2.17.1", target: "mods/core.jar", blocking: true },
      { id: "aws-access-key-id", kind: "secret", severity: "CRITICAL", target: "config/keys.txt",
        title: "AWS Access Key ID", blocking: true },
      { id: "CVE-2021-35515", kind: "vulnerability", severity: "HIGH", package: "org.apache.commons:commons-compress",
        installed: "1.5", fixed: "1.21", target: "paper/paper.jar", blocking: false, accepted: true },
      { id: "CVE-2024-0002", kind: "vulnerability", severity: "HIGH", package: "openssl",
        installed: "3.0.13", target: "usr/lib/libssl.so.3", blocking: false },
    ],
  },
};

const CLEAN: BuildScan = {
  build_id: "bld-8",
  scanned_at: "2026-09-20T10:00:00Z",
  has_report: true,
  has_sbom: true,
  summary: {
    policy: { fail_on: ["HIGH"], fail_unfixed: true },
    blocked: false,
    packages: 1,
    counts: {},
    blocking_counts: {},
    findings: [],
  },
};

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
});
afterEach(() => {
  vi.restoreAllMocks();
});

describe("BuildScanPanel", () => {
  it("shows what blocked the image, the listed findings, and how many more the report holds", async () => {
    calls.getBuildScan.mockResolvedValue(BLOCKED);
    render(<BuildScanPanel build={FAILED} />);
    const panel = await screen.findByRole("region", { name: "Security scan" });
    expect(calls.getBuildScan).toHaveBeenCalledWith("bld-7");
    expect(within(panel).getByText("Blocked")).toBeTruthy();
    expect(panel.textContent).toContain(
      "· 12 packages · Blocks on CRITICAL, HIGH; vulnerabilities with no fixed release are listed only · 1 ID accepted as a known risk",
    );
    expect(within(panel).getByText("1 ID accepted as a known risk").getAttribute("title")).toBe("CVE-2021-35515");

    const chips = within(within(panel).getByRole("list", { name: "Findings by severity" })).getAllByRole("listitem");
    expect(chips.map((c) => c.textContent)).toEqual(["CRITICAL2", "HIGH2", "MEDIUM2", "LOW0", "UNKNOWN0"]);
    expect(chips[0].getAttribute("title")).toBe("2 block the image");
    expect(chips[1].getAttribute("title")).toBeNull();

    const cve = within(panel).getByText("CVE-2024-0001").closest("li")!;
    expect(cve.textContent).toBe("CVE-2024-0001BlocksCRITICALlog4j-core2.14.1 → 2.17.1mods/core.jar");
    const secret = within(panel).getByText("aws-access-key-id").closest("li")!;
    expect(secret.textContent).toBe("aws-access-key-idBlocksCRITICALSecret: AWS Access Key ID—config/keys.txt");
    const accepted = within(panel).getByText("CVE-2021-35515").closest("li")!;
    expect(accepted.textContent).toBe("CVE-2021-35515AcceptedHIGHorg.apache.commons:commons-compress1.5 → 1.21paper/paper.jar");
    expect(within(accepted).getByText("Accepted").getAttribute("title")).toBe(
      "Listed in [registry] scan_accept as a known risk, so it never blocks.",
    );
    const unfixed = within(panel).getByText("CVE-2024-0002").closest("li")!;
    expect(unfixed.textContent).toBe("CVE-2024-0002HIGHopenssl3.0.13 → no fix yetusr/lib/libssl.so.3");
    expect(within(panel).getByText("Showing 4 of 6 findings. The full report lists every one.")).toBeTruthy();
  });

  it("downloads the report it kept and explains the SBOM it did not", async () => {
    calls.getBuildScan.mockResolvedValue(BLOCKED);
    calls.downloadBuildScanDocument.mockResolvedValue(undefined);
    render(<BuildScanPanel build={FAILED} />);
    const panel = await screen.findByRole("region", { name: "Security scan" });
    const sbom = within(panel).getByRole("button", { name: "SBOM" }) as HTMLButtonElement;
    expect(sbom.disabled).toBe(true);
    expect(sbom.getAttribute("title")).toBe(
      "Not kept with this build: the file was too large, or the step that writes it failed.",
    );
    await userEvent.click(within(panel).getByRole("button", { name: "Trivy report" }));
    expect(calls.downloadBuildScanDocument).toHaveBeenCalledWith("bld-7", "report");

    calls.downloadBuildScanDocument.mockRejectedValue({
      status: 404,
      code: "scan_document_not_kept",
      message: "this build's scan kept no report: it was too large to keep, or the step that writes it failed",
    });
    await userEvent.click(within(panel).getByRole("button", { name: "Trivy report" }));
    expect((await within(panel).findByRole("alert")).textContent).toBe(
      "Couldn't download: This build kept no copy of that scan file: it was too large to keep, or the step that writes it failed.",
    );
  });

  it("says a clean scan found nothing and names the stricter policy it ran under", async () => {
    calls.getBuildScan.mockResolvedValue(CLEAN);
    render(<BuildScanPanel build={{ ...FAILED, id: "bld-8", status: "succeeded" }} />);
    const panel = await screen.findByRole("region", { name: "Security scan" });
    expect(within(panel).getByText("Passed")).toBeTruthy();
    expect(panel.textContent).toContain("· 1 package · Blocks on HIGH, including vulnerabilities with no fixed release");
    expect(panel.textContent).not.toContain("accepted");
    expect(within(panel).getByText("Trivy found nothing to report in this image.")).toBeTruthy();
    expect(within(panel).queryByText("Found in")).toBeNull();
    calls.downloadBuildScanDocument.mockResolvedValue(undefined);
    await userEvent.click(within(panel).getByRole("button", { name: "SBOM" }));
    expect(calls.downloadBuildScanDocument).toHaveBeenCalledWith("bld-8", "sbom");
  });

  it("says when a build kept no scan, and retries a scan that failed to load", async () => {
    calls.getBuildScan.mockRejectedValue({ status: 404, code: "scan_not_found", message: "this build has no scan" });
    const { unmount } = render(<BuildScanPanel build={FAILED} />);
    expect(
      await screen.findByText(
        "No security scan was kept for this build. It stopped before the scan step, or it ran before builds kept their scans.",
      ),
    ).toBeTruthy();
    unmount();

    calls.getBuildScan.mockRejectedValueOnce({ status: 500, code: "internal", message: "database is away" });
    calls.getBuildScan.mockResolvedValueOnce(BLOCKED);
    render(<BuildScanPanel build={FAILED} />);
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Couldn't load the security scan: The operation failed because of an internal server error. Ask an admin to check the logs.",
    );
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("region", { name: "Security scan" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("asks nothing for a build that is still running or was cancelled", () => {
    const { container, rerender } = render(<BuildScanPanel build={{ ...FAILED, status: "building" }} />);
    rerender(<BuildScanPanel build={{ ...FAILED, status: "cancelled" }} />);
    rerender(<BuildScanPanel build={{ ...FAILED, status: "pending" }} />);
    expect(container.textContent).toBe("");
    expect(calls.getBuildScan).not.toHaveBeenCalled();
  });
});
