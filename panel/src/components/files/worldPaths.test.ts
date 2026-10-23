import { describe, expect, it } from "vitest";
import { PropertiesParseError } from "./properties";
import { touchesWorld, worldDirectories } from "./worldPaths";

describe("running world paths", () => {
  it.each([
    ["", "world/region/r.0.0.mca"],
    ["# level-name=ignored\nlevel-name=survival", "survival_nether/level.dat"],
    ["level-name : maps/custom", "maps/custom_the_end/level.dat"],
    ["level-name=my\\ world", "my world/level.dat"],
    ["level-name=\\u4e16\\u754c", "世界/level.dat"],
    ["level-name=old\nlevel-name=./maps/../new", "new/level.dat"],
    ["level-name=my\\\n  world", "myworld/level.dat"],
    ["motd=other\rlevel-name=maps/current\r", "maps/current/region/r.0.0.mca"],
    ["level\\u002dname=escaped", "escaped_nether/level.dat"],
    ["level\\-name=escaped-key", "escaped-key_the_end/level.dat"],
    ["level-\\\r\n  name=continued-key", "continued-key/level.dat"],
  ])("recognizes world files from %s", (properties, path) => {
    expect(touchesWorld(path, worldDirectories(properties))).toBe(true);
    expect(touchesWorld("plugins/config.yml", worldDirectories(properties))).toBe(false);
    expect(touchesWorld("server.properties", worldDirectories(properties))).toBe(false);
  });

  it("protects parent directory changes and keeps similar names separate", () => {
    const dirs = worldDirectories("level-name=maps/game");
    expect(touchesWorld("maps", dirs)).toBe(true);
    expect(touchesWorld("world", dirs)).toBe(true);
    expect(touchesWorld("world_backup/level.dat", dirs)).toBe(false);
    expect(touchesWorld("maps/other", dirs)).toBe(false);
  });

  it("conservatively protects the root when level-name is explicitly empty", () => {
    const dirs = worldDirectories("level-name=\r\n");
    expect(dirs).toContain("");
    expect(touchesWorld("plugins/config.yml", dirs)).toBe(true);
    expect(touchesWorld("server.properties", dirs)).toBe(true);
  });

  it.each([
    "level-name=\\uNOPE",
    "level-name=world\nother=\\u123",
    "level\\uXX2dname=custom",
  ])("reports malformed Unicode so confirmation can remain conservative", (properties) => {
    expect(() => worldDirectories(properties)).toThrow(PropertiesParseError);
  });
});
