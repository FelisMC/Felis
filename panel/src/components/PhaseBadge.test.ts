import { describe, it, expect } from "vitest";
import { PHASE_COLOR, pendingPower, phaseColor, phaseVariant, shownPhase, startFailure } from "@/components/PhaseBadge";
import type { Phase } from "@/lib/types";

// The closed lifecycle set felis-api can emit — the Go source of truth is
// internal/apis/felis/v1alpha1 MinecraftServerPhase (Unknown/Stopped/Starting/
// Running/Stopping/Failed), passed through verbatim by the status projection
// (internal/api/k8scluster.go). `satisfies readonly Phase[]` is the real guard:
// it fails the build if any of these strings drops out of the TS Phase union, the
// exact desync that left a "Stopping" server rendering with no variant/colour.
// tsc enforces map↔union agreement, but ONLY this pins union↔backend agreement.
const BACKEND_PHASES = [
  "Unknown",
  "Stopped",
  "Starting",
  "Running",
  "Stopping",
  "Failed",
] as const satisfies readonly Phase[];

describe("phase palette parity", () => {
  it("resolves every backend phase to a concrete colour and variant", () => {
    for (const phase of BACKEND_PHASES) {
      expect(phaseColor(phase), `colour for ${phase}`).toMatch(/^#[0-9a-f]{6}$/i);
      expect(phaseVariant(phase), `variant for ${phase}`).toBeTruthy();
    }
  });

  it("gives every phase a distinct hue so transitions are legible", () => {
    const hues = BACKEND_PHASES.map((p) => phaseColor(p));
    expect(new Set(hues).size).toBe(BACKEND_PHASES.length);
  });
});

describe("unmodelled phase fallback", () => {
  // `phase` crosses an unvalidated JSON boundary (api.status() is parsed, never
  // schema-checked), so the backend emitting a phase the panel predates must NOT
  // produce an undefined variant/colour — it degrades to the neutral Unknown
  // styling. Casting simulates that out-of-union runtime value.
  const future = "Hibernating" as Phase;

  it("degrades an out-of-union phase to the Unknown colour", () => {
    expect(phaseColor(future)).toBe(PHASE_COLOR.Unknown);
  });

  it("degrades an out-of-union phase to the Unknown variant", () => {
    expect(phaseVariant(future)).toBe(phaseVariant("Unknown"));
  });
});

describe("start failure", () => {
  // A Failed phase only reads as a failed start while the server still wants to
  // run; a stopped Failed server needs nothing from its owner.
  it("reads a failed start only while the server wants to run", () => {
    expect(startFailure({ phase: "Running", desiredState: "Running" })).toBeNull();
    expect(startFailure({ phase: "Failed", desiredState: "Stopped" })).toBeNull();
    expect(startFailure({ phase: "Failed" })).toBeNull();
    expect(startFailure({ phase: "Failed", desiredState: "Running" })).toBe("retrying");
    expect(startFailure({ phase: "Failed", desiredState: "Running", startGaveUp: true })).toBe("gaveUp");
  });
});

describe("a server asked to move", () => {
  // desiredState flips the moment a wake or stop is accepted; the phase follows
  // once the operator acts. Every pair the two can be in:
  const cases: [Phase, "Running" | "Stopped" | undefined, "start" | "stop" | null, Phase][] = [
    ["Stopped", "Running", "start", "Starting"],
    ["Unknown", "Running", "start", "Starting"],
    // Woken while going down: it comes back up once it is down.
    ["Stopping", "Running", "start", "Starting"],
    ["Starting", "Running", null, "Starting"],
    ["Running", "Running", null, "Running"],
    // A failed start keeps its own badge and retry.
    ["Failed", "Running", null, "Failed"],
    ["Running", "Stopped", "stop", "Stopping"],
    ["Starting", "Stopped", "stop", "Stopping"],
    ["Stopping", "Stopped", null, "Stopping"],
    ["Stopped", "Stopped", null, "Stopped"],
    ["Failed", "Stopped", null, "Failed"],
    ["Unknown", "Stopped", null, "Unknown"],
    // A view without desiredState shows the phase as it is.
    ["Stopped", undefined, null, "Stopped"],
    ["Running", undefined, null, "Running"],
  ];

  for (const [phase, desiredState, pending, shown] of cases) {
    it(`${phase} asked ${desiredState ?? "(unknown)"} is pending ${pending} and shows ${shown}`, () => {
      expect(pendingPower({ phase, desiredState })).toBe(pending);
      expect(shownPhase({ phase, desiredState })).toBe(shown);
    });
  }
});
