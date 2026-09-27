import { describe, it, expect } from "vitest";
import { isManaged, joinPath, nameProblem, parentOf, sortEntries } from "./names";
import type { ServerFileEntry } from "@/lib/types";

const entry = (name: string, is_dir = false): ServerFileEntry => ({ name, size: 1, is_dir, mod_time: "2026-09-01T00:00:00Z" });

describe("joinPath and parentOf", () => {
  it("join at the root without a leading slash, and walk back to it", () => {
    expect(joinPath("", "world")).toBe("world");
    expect(joinPath("world/region", "r.0.0.mca")).toBe("world/region/r.0.0.mca");
    expect(parentOf("world/region")).toBe("world");
    expect(parentOf("world")).toBe("");
  });
});

describe("isManaged", () => {
  it("names exactly the paths the read path guards by name", () => {
    expect(isManaged("server.properties")).toBe(true);
    expect(isManaged("config")).toBe(true);
    expect(isManaged("config/paper-global.yml")).toBe(true);
    // The same names anywhere else are the owner's own.
    expect(isManaged("backup/server.properties")).toBe(false);
    expect(isManaged("config/paper-world-defaults.yml")).toBe(false);
    expect(isManaged("plugins/config")).toBe(false);
  });
});

describe("nameProblem", () => {
  const here = [entry("world", true), entry("eula.txt")];

  it("takes a free name, less the spaces around it", () => {
    expect(nameProblem("  notes.txt ", here)).toBeNull();
  });

  it("asks for a name when there is none but spaces", () => {
    expect(nameProblem("", here)).toBe("name_required");
    expect(nameProblem("   ", here)).toBe("name_required");
  });

  it("refuses a slash, which would reach into another folder", () => {
    expect(nameProblem("config/x", here)).toBe("name_slash");
  });

  it("refuses the names that mean this folder or its parent", () => {
    expect(nameProblem(".", here)).toBe("name_dots");
    expect(nameProblem(" .. ", here)).toBe("name_dots");
    expect(nameProblem("...", here)).toBeNull();
    expect(nameProblem(".hidden", here)).toBeNull();
  });

  it("counts the length in bytes, as the filesystem does", () => {
    expect(nameProblem("a".repeat(255), here)).toBeNull();
    expect(nameProblem("a".repeat(256), here)).toBe("name_too_long");
    // 85 three-byte characters are 255 bytes; one more is over.
    expect(nameProblem("界".repeat(85), here)).toBeNull();
    expect(nameProblem("界".repeat(86), here)).toBe("name_too_long");
  });

  it("refuses a name already in the folder, file or folder alike", () => {
    expect(nameProblem("eula.txt", here)).toBe("name_taken");
    expect(nameProblem(" world", here)).toBe("name_taken");
    expect(nameProblem("World", here)).toBeNull();
  });

  it("lets a rename keep its own name, and no other one there", () => {
    expect(nameProblem("world", here, "world")).toBeNull();
    expect(nameProblem("eula.txt", here, "world")).toBe("name_taken");
  });

  it("checks nothing against a folder not listed yet", () => {
    expect(nameProblem("eula.txt", null)).toBeNull();
  });
});

describe("sortEntries", () => {
  it("puts folders first, then orders names by their numbers and without case", () => {
    const listed = [
      entry("r.10.0.mca"),
      entry("world", true),
      entry("Banned-players.json"),
      entry("r.2.0.mca"),
      entry("logs", true),
      entry("apple.txt"),
    ];
    expect(sortEntries(listed).map((e) => e.name)).toEqual([
      "logs",
      "world",
      "apple.txt",
      "Banned-players.json",
      "r.2.0.mca",
      "r.10.0.mca",
    ]);
    // The listing it was given stays as the server sent it.
    expect(listed[0].name).toBe("r.10.0.mca");
  });
});
