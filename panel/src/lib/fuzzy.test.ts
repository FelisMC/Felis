import { describe, it, expect } from "vitest";
import { fuzzyScore, matchScore } from "./fuzzy";

describe("fuzzyScore", () => {
  it("matches a contiguous substring and scores it far above a subsequence", () => {
    const substring = fuzzyScore("survival", "surv");
    const subsequence = fuzzyScore("survival", "srv");
    expect(substring).toBeGreaterThan(0);
    expect(subsequence).toBeGreaterThan(0);
    // "surv" is contiguous (substring) → must outrank the "srv" subsequence.
    expect(substring).toBeGreaterThan(subsequence);
  });

  it("is case-insensitive", () => {
    expect(fuzzyScore("Survival SMP", "smp")).toBeGreaterThan(0);
    expect(fuzzyScore("survival", "SURV")).toBeGreaterThan(0);
  });

  it("matches a non-contiguous subsequence (srv → survival)", () => {
    expect(fuzzyScore("survival", "srv")).toBeGreaterThan(0);
    expect(fuzzyScore("survival", "sl")).toBeGreaterThan(0);
  });

  it("returns -1 when a query char is absent or out of order", () => {
    expect(fuzzyScore("creative", "surv")).toBe(-1); // no 's' in creative
    expect(fuzzyScore("survival", "lavivrus")).toBe(-1); // right chars, wrong order
  });

  it("ranks an earlier / word-boundary hit above a later one", () => {
    // "lab" at the start of the second word beats "ab" buried mid-word.
    const boundary = fuzzyScore("creative lab", "lab");
    const buried = fuzzyScore("xlabx", "lab");
    expect(boundary).toBeGreaterThan(buried);
  });

  it("treats an empty query as a neutral match and empty text as no match", () => {
    expect(fuzzyScore("anything", "")).toBe(0);
    expect(fuzzyScore("", "x")).toBe(-1);
  });
});

describe("matchScore", () => {
  const fields = ["survival", "survival", "owner@mock.felis.local"];

  it("requires every term to hit some field (AND across terms)", () => {
    // Both "owner" (owner field) and "surv" (name) match → included.
    expect(matchScore(fields, ["owner", "surv"])).toBeGreaterThan(0);
    // "owner" matches but "zzz" matches nothing → the whole record is rejected.
    expect(matchScore(fields, ["owner", "zzz"])).toBe(-1);
  });

  it("matches a term against any field (OR across fields)", () => {
    expect(matchScore(fields, ["mock"])).toBeGreaterThan(0); // only in the owner field
    expect(matchScore(fields, ["survival"])).toBeGreaterThan(0); // only in the name field
  });

  it("sums per-term best scores, so more matched terms rank higher", () => {
    const one = matchScore(fields, ["surv"]);
    const two = matchScore(fields, ["surv", "owner"]);
    expect(two).toBeGreaterThan(one);
  });
});
