export type ConfigFieldSpec = { id: string; path: readonly string[]; hint?: string } & (
  | { type: "boolean" }
  | { type: "integer"; min?: number; max?: number; multipleOf?: number }
  | { type: "number"; min: number; allowMinusOne?: boolean }
  | { type: "text"; maxBytes?: number }
  | { type: "enum"; options: readonly (string | number)[] }
);

export function configProblem(field: ConfigFieldSpec, value: string): string | undefined {
  if (field.type === "boolean" && !["true", "false"].includes(value)) return "boolean";
  if (field.type === "enum" && !field.options.some((option) => String(option) === value)) return "enum";
  if (field.type === "integer") {
    if (!/^[+-]?\d+$/.test(value)) return field.min !== undefined ? "integer_range" : "integer";
    const integer = BigInt(value);
    if ((field.min !== undefined && integer < BigInt(field.min)) || (field.max !== undefined && integer > BigInt(field.max))) return "integer_range";
    if (field.multipleOf && integer % BigInt(field.multipleOf) !== 0n) return "multiple";
  }
  if (field.type === "number") {
    const number = Number(value);
    if (value.trim() === "" || !Number.isFinite(number) || (number < field.min && !(field.allowMinusOne && number === -1))) return "number";
  }
  if (field.type === "text" && field.maxBytes !== undefined && (value.includes("\0") || new TextEncoder().encode(value).length > field.maxBytes)) return "text";
}

export function configScalar(field: ConfigFieldSpec, value: string): string | number | boolean | bigint {
  if (configProblem(field, value)) return value;
  if (field.type === "boolean") return value === "true";
  if (field.type === "integer" || (field.type === "enum" && typeof field.options[0] === "number")) return BigInt(value);
  if (field.type === "number") return Number(value);
  return value;
}

export function yamlCompatible(field: ConfigFieldSpec, value: unknown): boolean {
  if (field.type === "boolean") return typeof value === "boolean";
  if (field.type === "integer") return typeof value === "bigint";
  if (field.type === "text") return typeof value === "string";
  if (field.type === "number") return typeof value === "number" && Number.isFinite(value);
  return field.options.some((option) => typeof option === "number" ? value === BigInt(option) : value === option);
}
