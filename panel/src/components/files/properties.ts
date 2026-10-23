type Property = {
  key: string;
  value: string;
  start: number;
  end: number;
  prefix: string;
  ending: string;
  pending: boolean;
  empty: boolean;
};

export class PropertiesParseError extends Error {
  constructor() {
    super("Invalid Unicode escape in properties file");
    this.name = "PropertiesParseError";
  }
}

function whitespace(char: string | undefined): boolean {
  return char === " " || char === "\t" || char === "\f";
}

function decode(value: string): string {
  let decoded = "";
  for (let i = 0; i < value.length; i++) {
    if (value[i] !== "\\") {
      decoded += value[i];
      continue;
    }
    const char = value[++i];
    if (char === "u") {
      const hex = value.slice(i + 1, i + 5);
      if (!/^[\da-fA-F]{4}$/.test(hex)) throw new PropertiesParseError();
      decoded += String.fromCharCode(parseInt(hex, 16));
      i += 4;
    } else {
      decoded += ({ t: "\t", n: "\n", r: "\r", f: "\f" } as Record<string, string>)[char] ?? char;
    }
  }
  return decoded;
}

function encode(value: string, key = false): string {
  return value.replace(/[\\\u0000-\u0020:=#!]/g, (char, index: number) => {
    switch (char) {
      case "\\": return "\\\\";
      case "\t": return "\\t";
      case "\n": return "\\n";
      case "\r": return "\\r";
      case "\f": return "\\f";
      case " ": return key || index === 0 ? "\\ " : char;
      case ":": case "=": case "#": case "!": return key ? "\\" + char : char;
      default: return "\\u" + char.charCodeAt(0).toString(16).padStart(4, "0");
    }
  });
}

function property(logical: string, start: number, end: number, indent: string, ending: string, pending: boolean): Property {
  let keyEnd = 0;
  while (keyEnd < logical.length) {
    const char = logical[keyEnd];
    if (char === "\\") {
      keyEnd += 2;
    } else if (whitespace(char) || char === "=" || char === ":") {
      break;
    } else {
      keyEnd++;
    }
  }
  let valueStart = keyEnd;
  while (whitespace(logical[valueStart])) valueStart++;
  if (logical[valueStart] === "=" || logical[valueStart] === ":") valueStart++;
  while (whitespace(logical[valueStart])) valueStart++;
  return {
    key: decode(logical.slice(0, keyEnd)),
    value: decode(logical.slice(valueStart)),
    start, end,
    prefix: indent + logical.slice(0, valueStart) + (keyEnd === logical.length ? "=" : ""),
    ending, pending,
    empty: logical.length === 0,
  };
}

function* properties(text: string): Generator<Property> {
  let logical = "";
  let start = 0;
  let end = 0;
  let indent = "";
  let ending = "";
  let pending = false;
  for (const line of text.matchAll(/([^\r\n]*)(\r\n|\r|\n|$)/g)) {
    if (line[0] === "") break;
    const leading = /^[ \t\f]*/.exec(line[1])![0];
    const body = line[1].slice(leading.length);
    if (logical === "") {
      pending = false;
      if (body === "" || body[0] === "#" || body[0] === "!") continue;
      start = line.index;
      indent = leading;
    }
    const slashes = /\\+$/.exec(body)?.[0].length ?? 0;
    pending = slashes % 2 === 1;
    logical += pending ? body.slice(0, -1) : body;
    end = line.index + line[0].length;
    ending = line[2];
    if (pending && ending !== "") continue;
    yield property(logical, start, end, indent, ending, pending);
    logical = "";
    pending = false;
  }
  if (pending) yield property(logical, start, end, indent, ending, true);
}

export function readProperties(text: string): ReadonlyMap<string, string> {
  const values = new Map<string, string>();
  for (const entry of properties(text)) values.set(entry.key, entry.value);
  return values;
}

export function getProperty(text: string, key: string): string | undefined {
  let value: string | undefined;
  for (const entry of properties(text)) {
    if (entry.key === key) value = entry.value;
  }
  return value;
}

export function setProperty(text: string, key: string, value: string): string {
  let found: Property | undefined;
  let last: Property | undefined;
  for (const entry of properties(text)) {
    last = entry;
    if (entry.key === key) found = entry;
  }
  if (found?.value === value) return text;
  const encoded = encode(value);
  if (found) {
    return text.slice(0, found.start) + found.prefix + encoded + found.ending + text.slice(found.end);
  }
  const newline = /\r\n|\r|\n/.exec(text)?.[0] ?? "\n";
  const terminated = /[\r\n]$/.test(text);
  const separator = text !== "" && !terminated ? newline : "";
  // A trailing continuation must finish before an appended property can start.
  const close = last?.end === text.length && last.pending ? (last.empty ? "=" : "") + newline : "";
  return text + separator + close + encode(key, true) + "=" + encoded + (terminated ? newline : "");
}
