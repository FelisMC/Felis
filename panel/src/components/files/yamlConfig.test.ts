import { describe, expect, it } from "vitest";
import { getYamlScalar, readYamlConfig, setYamlScalar, YamlConfigParseError } from "./yamlConfig";

describe("YAML configuration drafts", () => {
  it("reads YAML 1.1 booleans and integers without losing precision", () => {
    const document = readYamlConfig("settings:\n  enabled: yes\n  disabled: OFF\n  octal: 012\n  exact: 9007199254740993\n");
    expect(getYamlScalar(document, ["settings", "enabled"])).toBe(true);
    expect(getYamlScalar(document, ["settings", "disabled"])).toBe(false);
    expect(getYamlScalar(document, ["settings", "octal"])).toBe(10n);
    expect(getYamlScalar(document, ["settings", "exact"])).toBe(9007199254740993n);
  });

  it("uses YAML 1.2 scalar types for the protected configuration mock", () => {
    const source = "legacy: yes\nenabled: true\nlimit: 20\n";
    const document = readYamlConfig(source, "1.2");
    expect(getYamlScalar(document, ["legacy"])).toBe("yes");
    expect(getYamlScalar(document, ["enabled"])).toBe(true);
    const changed = setYamlScalar(source, ["limit"], 30n, "1.2");
    expect(getYamlScalar(readYamlConfig(changed, "1.2"), ["legacy"])).toBe("yes");
    expect(getYamlScalar(readYamlConfig(changed, "1.2"), ["limit"])).toBe(30n);
  });

  it.each([
    ["# keep\r\nsettings: { enabled: yes } # tail", ["settings", "enabled"], true],
    ["limit: 012\n", ["limit"], 10n],
    ["limit: !!int '20'", ["limit"], 20],
    ["limit: 2.0\n", ["limit"], 2n],
    ["# keep\rlimit: 20\runknown: retained", ["limit"], 20n],
    ["message: '世界' # keep\n", ["message"], "世界"],
  ] as const)("keeps a semantic no-op byte-for-byte in %j", (source, path, value) => {
    expect(setYamlScalar(source, path, value)).toBe(source);
  });

  it("retains comments, unknown structures, scalar data and order when changing one setting", () => {
    const source = "# heading\nsettings:\n  # limit guidance\n  limit: 20 # inline\nunknown:\n  exact: 9007199254740993\n  items: [one, two]\n  nested:\n    value: 'keep: # exact'\n# footer\n";
    const changed = setYamlScalar(source, ["settings", "limit"], 30n);
    for (const comment of ["# heading", "# limit guidance", "# inline", "# footer"]) expect(changed).toContain(comment);
    expect(changed.indexOf("settings:")).toBeLessThan(changed.indexOf("unknown:"));
    const parsed = readYamlConfig(changed);
    expect(getYamlScalar(parsed, ["settings", "limit"])).toBe(30n);
    expect(getYamlScalar(parsed, ["unknown", "exact"])).toBe(9007199254740993n);
    expect(getYamlScalar(parsed, ["unknown", "nested", "value"])).toBe("keep: # exact");
    expect(parsed.getIn(["unknown", "items", 1])).toBe("two");
  });

  it("treats dots and namespaces in mapping keys as literal path segments", () => {
    const source = "world-settings:\n  my.world:\n    limit: 20\n  minecraft:overworld:\n    limit: 40\n  default:\n    limit: 10\n";
    const changed = setYamlScalar(source, ["world-settings", "my.world", "limit"], 30n);
    const parsed = readYamlConfig(changed);
    expect(getYamlScalar(parsed, ["world-settings", "my.world", "limit"])).toBe(30n);
    expect(getYamlScalar(parsed, ["world-settings", "minecraft:overworld", "limit"])).toBe(40n);
    expect(getYamlScalar(parsed, ["world-settings", "default", "limit"])).toBe(10n);
  });

  it.each(["\n", "\r\n", "\r"])("keeps %j line endings and a missing final newline", (newline) => {
    for (const terminated of [false, true]) {
      const source = ["# heading", "limit: 20 # inline", "unknown: retained"].join(newline) + (terminated ? newline : "");
      const changed = setYamlScalar(source, ["limit"], 30n);
      expect(/[\r\n]$/.test(changed)).toBe(terminated);
      expect(changed.replaceAll(newline, "")).not.toMatch(/[\r\n]/);
      expect(getYamlScalar(readYamlConfig(changed), ["limit"])).toBe(30n);
      expect(getYamlScalar(readYamlConfig(changed), ["unknown"])).toBe("retained");
    }
  });

  it("keeps invalid numeric drafts editable and can restore the original scalar type", () => {
    const source = "limit: !!int '20' # guidance\n";
    const draft = setYamlScalar(source, ["limit"], "not an integer");
    expect(getYamlScalar(readYamlConfig(draft), ["limit"])).toBe("not an integer");
    const fixed = setYamlScalar(draft, ["limit"], 24n);
    expect(getYamlScalar(readYamlConfig(fixed), ["limit"])).toBe(24n);
    expect(fixed).toContain("# guidance");
  });

  it("escapes multiline text without creating extra settings", () => {
    const value = "First line\nsecond: text\n# literal\n世界";
    const changed = setYamlScalar("message: before\nunknown: retained\n", ["message"], value);
    expect(getYamlScalar(readYamlConfig(changed), ["message"])).toBe(value);
    expect(getYamlScalar(readYamlConfig(changed), ["unknown"])).toBe("retained");
  });

  it("leaves missing and incompatible fields to text mode without constructing defaults", () => {
    const source = "settings:\n  nested: {value: one}\n  custom: !custom kept\n";
    const parsed = readYamlConfig(source);
    expect(getYamlScalar(parsed, ["settings", "missing"])).toBeUndefined();
    expect(getYamlScalar(parsed, ["settings", "nested"])).toBeUndefined();
    expect(getYamlScalar(parsed, ["settings", "custom"])).toBeUndefined();
    expect(() => setYamlScalar(source, ["settings", "missing"], false)).toThrow(YamlConfigParseError);
    expect(() => setYamlScalar(source, ["settings", "nested"], false)).toThrow(YamlConfigParseError);
    expect(readYamlConfig("").contents).toBeNull();
  });

  it.each([
    "settings: [",
    "limit: 1\nlimit: 2\n",
    "---\nlimit: 1\n---\nlimit: 2\n",
    "template: &base {limit: 1}\nsettings: *base\n",
    "template: &unused value\n",
    "settings:\n  <<: {limit: 1}\n",
    "? [complex, key]\n: value\n",
    "1: value\n",
    "[one, two]\n",
    "scalar\n",
  ])("refuses invalid or shared structures without including source in errors: %j", (source) => {
    expect(() => readYamlConfig(source)).toThrow(YamlConfigParseError);
    try {
      readYamlConfig(source);
    } catch (error) {
      expect((error as Error).message).toBe("Invalid or unsupported YAML configuration");
    }
  });

  it("bounds nesting and refuses nonfinite edits", () => {
    const source = Array.from({ length: 66 }, (_, level) => " ".repeat(level * 2) + "nested:").join("\n");
    expect(() => readYamlConfig(source)).toThrow(YamlConfigParseError);
    expect(() => setYamlScalar("limit: 1\n", ["limit"], Infinity)).toThrow(YamlConfigParseError);
  });
});
