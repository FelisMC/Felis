import { isAlias, isMap, isScalar, isSeq, parseDocument } from "yaml";

export type YamlScalarValue = string | number | boolean | bigint;
type YamlDocument = ReturnType<typeof parseDocument>;

export class YamlConfigParseError extends Error {
  constructor() {
    super("Invalid or unsupported YAML configuration");
    this.name = "YamlConfigParseError";
  }
}

function inspect(node: unknown, depth = 0): void {
  if (node === null || node === undefined) return;
  if (depth > 64 || isAlias(node)) throw new YamlConfigParseError();
  if (isScalar(node)) {
    if (node.anchor) throw new YamlConfigParseError();
    return;
  }
  if (isMap(node)) {
    if (node.anchor) throw new YamlConfigParseError();
    for (const pair of node.items) {
      if (!isScalar(pair.key) || typeof pair.key.value !== "string" || pair.key.value === "<<") throw new YamlConfigParseError();
      inspect(pair.value, depth + 1);
    }
    return;
  }
  if (isSeq(node)) {
    if (node.anchor) throw new YamlConfigParseError();
    node.items.forEach((item) => inspect(item, depth + 1));
    return;
  }
  throw new YamlConfigParseError();
}

export function readYamlConfig(text: string, version: "1.1" | "1.2" = "1.1"): YamlDocument {
  try {
    const document = parseDocument(text.replace(/\r\n?/g, "\n"), { version, intAsBigInt: true, uniqueKeys: true, prettyErrors: false });
    if (document.errors.length || (document.contents !== null && !isMap(document.contents))) throw new YamlConfigParseError();
    inspect(document.contents);
    return document;
  } catch (error) {
    if (error instanceof RangeError) throw new YamlConfigParseError();
    throw error;
  }
}

export function getYamlScalar(document: YamlDocument, path: readonly string[]): unknown {
  const node = document.getIn(path, true);
  if (!isScalar(node) || (node.tag && !/^tag:yaml\.org,2002:(?:str|bool|int|float|null)$/.test(node.tag))) return undefined;
  return node.value;
}

export function setYamlScalar(text: string, path: readonly string[], value: YamlScalarValue, version: "1.1" | "1.2" = "1.1"): string {
  const document = readYamlConfig(text, version);
  const node = document.getIn(path, true);
  if (!isScalar(node) || getYamlScalar(document, path) === undefined || (typeof value === "number" && !Number.isFinite(value))) throw new YamlConfigParseError();
  const integer = typeof node.value === "bigint" ? node.value : value;
  const numeric = typeof node.value === "number" ? node.value : value;
  if (Object.is(node.value, value) || (typeof integer === "bigint" && typeof numeric === "number" && Number.isSafeInteger(numeric) && integer === BigInt(numeric))) return text;
  node.value = value;
  node.tag = undefined;
  let output = document.toString({ lineWidth: 0 });
  if (!/[\r\n]$/.test(text)) output = output.slice(0, -1);
  const newline = /\r\n|\r|\n/.exec(text)?.[0] ?? "\n";
  return newline === "\n" ? output : output.replaceAll("\n", newline);
}
