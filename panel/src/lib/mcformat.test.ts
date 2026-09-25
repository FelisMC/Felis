import { describe, it, expect } from "vitest";
import { parseFormatting } from "./mcformat";

describe("parseFormatting", () => {
  it("returns a plain line untouched, without segments", () => {
    expect(parseFormatting("[12:00:00] [Server thread/INFO]: Done")).toEqual({
      text: "[12:00:00] [Server thread/INFO]: Done",
    });
  });

  it("colours text after a § colour code and resets on §r", () => {
    expect(parseFormatting("§aReady§r now")).toEqual({
      text: "Ready now",
      segments: [
        { text: "Ready", color: "#55ff55" },
        { text: " now" },
      ],
    });
  });

  it("reads codes in either case and lifts black so it shows on the console", () => {
    expect(parseFormatting("§Cred§0dark").segments).toEqual([
      { text: "red", color: "#ff5555" },
      { text: "dark", color: "#71717a" },
    ]);
  });

  it("stacks formatting codes until a colour code clears them", () => {
    expect(parseFormatting("§l§nTitle§6gold").segments).toEqual([
      { text: "Title", bold: true, underline: true },
      { text: "gold", color: "#ffaa00" },
    ]);
    expect(parseFormatting("§o§mold§r").segments).toEqual([{ text: "old", italic: true, strike: true }]);
  });

  it("keeps the colour when formatting is added after it", () => {
    expect(parseFormatting("§b§lBold aqua").segments).toEqual([
      { text: "Bold aqua", color: "#55ffff", bold: true },
    ]);
  });

  it("reads the §x hex form", () => {
    expect(parseFormatting("§x§F§f§8§8§0§0orange").segments).toEqual([
      { text: "orange", color: "#ff8800" },
    ]);
  });

  it("drops a broken §x, scrambled text and unknown codes without styling", () => {
    expect(parseFormatting("§xhi")).toEqual({ text: "hi" });
    expect(parseFormatting("§kmagic§z!")).toEqual({ text: "magic!" });
  });

  it("reads what follows a short §x as ordinary codes", () => {
    expect(parseFormatting("§x§f§fhi").segments).toEqual([{ text: "hi", color: "#ffffff" }]);
  });

  it("drops a § at the end of a line", () => {
    expect(parseFormatting("tail§")).toEqual({ text: "tail" });
  });

  it("omits segments when codes only reset", () => {
    expect(parseFormatting("§rplain§r")).toEqual({ text: "plain" });
  });

  it("maps ANSI SGR colours, bright colours and resets", () => {
    expect(parseFormatting("\x1b[31merror\x1b[0m ok \x1b[1;92mgreat\x1b[m done")).toEqual({
      text: "error ok great done",
      segments: [
        { text: "error", color: "#cd3131" },
        { text: " ok " },
        { text: "great", color: "#23d18b", bold: true },
        { text: " done" },
      ],
    });
  });

  it("maps 256-colour and true-colour escapes", () => {
    expect(parseFormatting("\x1b[38;5;196ma\x1b[38;5;67mb\x1b[38;5;244mc\x1b[38;2;1;2;3md").segments).toEqual([
      { text: "a", color: "#ff0000" },
      { text: "b", color: "#5f87af" },
      { text: "c", color: "#808080" },
      { text: "d", color: "#010203" },
    ]);
  });

  it("skips background colours along with their arguments", () => {
    expect(parseFormatting("\x1b[48;5;1;33mx")).toEqual({
      text: "x",
      segments: [{ text: "x", color: "#e5e510" }],
    });
    expect(parseFormatting("\x1b[33;48;2;9;9;9mx").segments).toEqual([{ text: "x", color: "#e5e510" }]);
  });

  it("turns single attributes off", () => {
    expect(parseFormatting("\x1b[1;4;36mon\x1b[22moff\x1b[24;39mplain").segments).toEqual([
      { text: "on", bold: true, underline: true, color: "#11a8cd" },
      { text: "off", underline: true, color: "#11a8cd" },
      { text: "plain" },
    ]);
  });

  it("drops cursor and erase escapes, and a lone ESC", () => {
    expect(parseFormatting("\x1b[2K\x1b[1Gline\x1b")).toEqual({ text: "line" });
  });

  it("never lets markup through as anything but text", () => {
    const out = parseFormatting('§c<img src=x onerror="alert(1)">');
    expect(out.text).toBe('<img src=x onerror="alert(1)">');
    expect(out.segments).toEqual([{ text: '<img src=x onerror="alert(1)">', color: "#ff5555" }]);
  });
});
