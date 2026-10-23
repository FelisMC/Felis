import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { parse } from "yaml";
import { configProblem, configScalar } from "./configFields";
import { YAML_FIELDS } from "./yamlFields";
import { PAPER_GLOBAL_FIELDS } from "./paperGlobalFields";

describe("configuration field catalogue", () => {
  it("keeps the protected form's IDs, literal paths, types and integer ranges aligned with the backend permission catalogue", () => {
    const source = readFileSync(new URL("../../../../internal/fileedit/paper_global.go", import.meta.url), "utf8");
    const declarations = [...source.matchAll(/^\s*"([^"]+)":\s*\{path: \[\]string\{([^}]+)\}, kind: (paperGlobal\w+)(?:, min: (-?\d+))?\},$/gm)];
    const backend = declarations.map(([, id, path, kind, min]) => ({ id,
      path: [...path.matchAll(/"([^"]+)"/g)].map((match) => match[1]),
      type: ({ paperGlobalBool: "boolean", paperGlobalInt: "integer", paperGlobalNumber: "number", paperGlobalString: "text" } as Record<string, string>)[kind],
      ...(kind === "paperGlobalInt" ? { min: Number(min ?? 0), max: 2147483647 } : {}),
    })).sort((a, b) => a.id.localeCompare(b.id));
    const panel = PAPER_GLOBAL_FIELDS.map((field) => ({ id: field.path.join("."), path: field.path, type: field.type,
      ...(field.type === "integer" ? { min: field.min, max: field.max } : {}),
    })).sort((a, b) => a.id.localeCompare(b.id));
    expect(backend).toHaveLength(20);
    expect(panel).toEqual(backend);
    const spec = parse(readFileSync(new URL("../../../../api/openapi.yaml", import.meta.url), "utf8"));
    const properties = spec.components.schemas.PaperGlobalValues.properties as Record<string, { type: string }>;
    expect(Object.keys(properties).sort()).toEqual(panel.map((field) => field.id).sort());
    for (const field of panel) expect(properties[field.id].type).toBe(field.type === "text" ? "string" : field.type);
  });

  it("has 16/18/18/18 ordinary fields and complete bilingual copy without inventing integer ranges", () => {
    expect(Object.fromEntries(Object.entries(YAML_FIELDS).map(([path, fields]) => [path, fields.length]))).toEqual({
      "bukkit.yml": 16, "spigot.yml": 18, "config/paper-world-defaults.yml": 18, "purpur.yml": 18,
    });
    const fields = [...Object.values(YAML_FIELDS).flat(), ...PAPER_GLOBAL_FIELDS];
    expect(new Set(fields.map((field) => field.id)).size).toBe(90);
    for (const language of ["en-US", "zh-CN"]) {
      const copy = JSON.parse(readFileSync(new URL(`../../i18n/resources/${language}/files.json`, import.meta.url), "utf8"));
      for (const field of fields) {
        expect(copy[`config_field_${field.id}`], field.id).toBeTruthy();
        if (field.hint) expect(copy[field.hint], field.hint).toBeTruthy();
      }
    }
    for (const field of Object.values(YAML_FIELDS).flat()) {
      if (field.type !== "integer") continue;
      expect(field.min).toBeUndefined();
      expect(field.max).toBeUndefined();
    }
  });

  it("preserves integer precision and validates only the documented scalar constraints", () => {
    const integer = YAML_FIELDS["bukkit.yml"].find((field) => field.id === "bukkit_monsters")!;
    expect(configScalar(integer, "9007199254740993")).toBe(9007199254740993n);
    expect(configProblem(integer, "-2147483649")).toBeUndefined();
    expect(configProblem(integer, "")).toBe("integer");
    expect(configProblem(integer, "1.5")).toBe("integer");
    const text = PAPER_GLOBAL_FIELDS.find((field) => field.id === "global_no_permission")!;
    expect(configProblem(text, "x\0y")).toBe("text");
    expect(configProblem(text, "界".repeat(1365))).toBeUndefined();
    expect(configProblem(text, "界".repeat(1366))).toBe("text");
  });
});
