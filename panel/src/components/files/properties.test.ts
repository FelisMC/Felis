import { describe, expect, it } from "vitest";
import { getProperty, PropertiesParseError, readProperties, setProperty } from "./properties";

// Semantic fixtures follow https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/util/Properties.html#load(java.io.Reader).
describe("Java properties reading", () => {
  it("reads all current values with the last duplicate taking effect", () => {
    expect(readProperties("motd=first\nmax-players=20\nmotd=last\n")).toEqual(new Map([
      ["motd", "last"],
      ["max-players", "20"],
    ]));
  });

  it.each<[string, string, string | undefined]>([
    ["Truth = Beauty", "Truth", "Beauty"],
    [" Truth:Beauty", "Truth", "Beauty"],
    ["Truth\t\f :  Beauty", "Truth", "Beauty"],
    ["motd hello world  ", "motd", "hello world  "],
    ["motd", "motd", ""],
    ["motd=", "motd", ""],
    ["=empty key", "", "empty key"],
    ["motd==a:b", "motd", "=a:b"],
    ["motd : :a=b", "motd", ":a=b"],
    ["! motd=ignored\n\t #motd=also ignored\n", "motd", undefined],
    ["# ignored\\\nmotd=present", "motd", "present"],
    ["motd=first\r\nmotd : last\r\n", "motd", "last"],
    ["motd=old\rm\\otd=new\r", "motd", "new"],
    ["\\#name\\!=value", "#name!", "value"],
    ["name\\ with\\ space\\:\\==value", "name with space:=", "value"],
    ["path=C:\\\\server\\\\world", "path", "C:\\server\\world"],
    ["motd=\\ leading space\\ ", "motd", " leading space "],
    ["motd=tab\\tline\\nreturn\\rfeed\\f", "motd", "tab\tline\nreturn\rfeed\f"],
    ["motd=\\z\\b\\0\\\"\\'", "motd", "zb0\"'"],
    ["\\u006d\\u006ftd=\\u4e16\\u754c\\uD83D\\uDE3A", "motd", "世界😺"],
    ["motd=\\\\u0041", "motd", "\\u0041"],
    ["motd=before#still value!", "motd", "before#still value!"],
    ["\u00a0motd=value", "\u00a0motd", "value"],
    ["motd=end\\", "motd", "end"],
    ["\\", "", ""],
  ])("reads %j with key %j", (text, key, value) => {
    expect(getProperty(text, key)).toBe(value);
  });

  it("joins continued values without adding spaces and skips indentation", () => {
    const text = "motd=first, \\\r\n\t\f second,\\\n  third\rnext=untouched\r";
    expect(getProperty(text, "motd")).toBe("first, second,third");
    expect(getProperty(text, "next")).toBe("untouched");
  });

  it("distinguishes odd and even runs of trailing backslashes", () => {
    const even = "motd=first" + "\\".repeat(2) + "\nnext=value\n";
    const odd = "motd=first" + "\\".repeat(3) + "\n  second\nnext=value\n";
    expect(getProperty(even, "motd")).toBe("first\\");
    expect(getProperty(odd, "motd")).toBe("first\\second");
    expect(getProperty(even, "next")).toBe("value");
    expect(getProperty(odd, "next")).toBe("value");
  });

  it("supports continued and escaped property keys", () => {
    expect(getProperty("max-\\\n  players : 24\n", "max-players")).toBe("24");
    expect(getProperty("max\\u002dplayers=12\nmax-players=32", "max-players")).toBe("32");
  });

  it("keeps comment indicators inside a continued value", () => {
    expect(getProperty("motd=hello\\\n  #world\\\n !again\n", "motd")).toBe("hello#world!again");
  });

  it("ends a continued value at a blank natural line", () => {
    const text = "motd=hello\\\n \t\f\r\nnext=value";
    expect(getProperty(text, "motd")).toBe("hello");
    expect(getProperty(text, "next")).toBe("value");
  });

  it("does not promote empty continuations into unrelated keys or comments", () => {
    const text = "\\\n # comment\n\\\n \nnext=value";
    expect(getProperty(text, "")).toBeUndefined();
    expect(getProperty(text, "next")).toBe("value");
  });

  it.each(["\\u", "\\u123", "\\u12xz", "\\uu0041"])("rejects malformed Unicode %j", (value) => {
    expect(() => getProperty("motd=" + value, "motd")).toThrow(PropertiesParseError);
    expect(() => getProperty(value + "=value", "motd")).toThrow(PropertiesParseError);
    expect(() => setProperty("motd=old\nother=" + value, "motd", "new")).toThrow(PropertiesParseError);
  });

  it("validates the entire file while ignoring escapes inside comments", () => {
    expect(() => getProperty("motd=valid\nother=\\uNOPE", "motd")).toThrow(PropertiesParseError);
    expect(getProperty("# \\uNOPE\n! \\u\nmotd=valid", "motd")).toBe("valid");
  });
});

describe("local property editing", () => {
  it.each([
    ["# comment\r\nmax-players : 20\r\n", "max-players", "20"],
    ["motd=\\u4e16\\u754c\n", "motd", "世界"],
    ["motd=one\\\n  two\n", "motd", "onetwo"],
    ["motd=first\nmotd=last", "motd", "last"],
    ["motd :", "motd", ""],
    ["motd=\\q", "motd", "q"],
    ["motd=end\\", "motd", "end"],
  ])("preserves %j byte-for-byte when its semantic value is unchanged", (text, key, value) => {
    expect(setProperty(text, key, value)).toBe(text);
  });

  it("changes only the last effective duplicate and preserves surrounding source", () => {
    const text = "# heading\r\nmax-players=12\r\n\r\n! keep this\r\n\tmax\\u002dplayers \t:  20\r\nplugin.setting=unknown\\ value\r\n";
    const want = "# heading\r\nmax-players=12\r\n\r\n! keep this\r\n\tmax\\u002dplayers \t:  30\r\nplugin.setting=unknown\\ value\r\n";
    expect(setProperty(text, "max-players", "30")).toBe(want);
  });

  it("replaces one continued logical line without rewriting later comments", () => {
    const text = "# heading\r\n  motd : old\\\r\n \t value\r\n! keep\ncustom : exact  \r\n";
    const want = "# heading\r\n  motd : new\r\n! keep\ncustom : exact  \r\n";
    expect(setProperty(text, "motd", "new")).toBe(want);
  });

  it("edits a continued key while retaining its separator", () => {
    const text = "# keep\n max-\\\r\n  players \t= 24\r\nunknown=value";
    expect(setProperty(text, "max-players", "40")).toBe("# keep\n max-players \t= 40\r\nunknown=value");
  });

  it.each([
    ["motd", "motd=new"],
    [" motd\t ", " motd\t new"],
    ["motd=old", "motd=new"],
    ["motd=old\n", "motd=new\n"],
    ["motd=old\r", "motd=new\r"],
  ])("retains separators and the final line ending in %j", (text, want) => {
    expect(setProperty(text, "motd", "new")).toBe(want);
  });

  it.each([
    ["", "max-players=20"],
    ["# keep", "# keep\nmax-players=20"],
    ["# keep\n", "# keep\nmax-players=20\n"],
    ["# keep\r\n\r\n", "# keep\r\n\r\nmax-players=20\r\n"],
    ["unknown=value\r", "unknown=value\rmax-players=20\r"],
    ["unknown=value\r\n# keep", "unknown=value\r\n# keep\r\nmax-players=20"],
  ])("appends a missing property without rewriting %j", (text, want) => {
    expect(setProperty(text, "max-players", "20")).toBe(want);
  });

  it("treats an explicit empty value as distinct from a missing key", () => {
    expect(getProperty("# keep\n", "motd")).toBeUndefined();
    expect(setProperty("# keep\n", "motd", "")).toBe("# keep\nmotd=\n");
  });

  it("escapes literal backslashes, leading whitespace, line breaks and controls", () => {
    const value = "  tab\tline\nreturn\rfeed\fzero\0end\\";
    const want = "motd=\\  tab\\tline\\nreturn\\rfeed\\fzero\\u0000end\\\\\nunknown=value\n";
    expect(setProperty("motd=old\nunknown=value\n", "motd", value)).toBe(want);
    expect(getProperty(want, "motd")).toBe(value);
    expect(getProperty(want, "unknown")).toBe("value");
  });

  it("escapes a new key so separators and comment indicators cannot create extra properties", () => {
    const key = " #name!:=\t\\";
    const want = "\\ \\#name\\!\\:\\=\\t\\\\=value";
    expect(setProperty("", key, "value")).toBe(want);
    expect(getProperty(want, key)).toBe("value");
  });

  it.each([
    ["motd=end\\", "motd=end\\\n\nmax-players=20"],
    ["motd=end\\\r\n", "motd=end\\\r\n\r\nmax-players=20\r\n"],
    ["\\", "\\\n=\nmax-players=20"],
  ])("closes a trailing continuation before appending to %j", (text, want) => {
    expect(setProperty(text, "max-players", "20")).toBe(want);
    expect(getProperty(want, "max-players")).toBe("20");
    expect(getProperty(want, "motd")).toBe(getProperty(text, "motd"));
    expect(getProperty(want, "")).toBe(getProperty(text, ""));
  });

  it("does not consume an unrelated trailing comment when updating a later key", () => {
    const text = "\\\n# comment\\\nmax-players=20\n";
    expect(setProperty(text, "max-players", "30")).toBe("\\\n# comment\\\nmax-players=30\n");
  });
});
