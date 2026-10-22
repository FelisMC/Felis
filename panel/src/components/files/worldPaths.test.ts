import { describe, expect, it } from "vitest";
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
});
