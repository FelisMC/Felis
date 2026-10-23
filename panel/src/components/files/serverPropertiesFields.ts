export const PROPERTY_GROUPS = ["game", "players", "world", "network", "performance"] as const;

type PropertyField = {
  key: string;
  group: typeof PROPERTY_GROUPS[number];
  managed?: "platform" | "login";
  loginOnly?: boolean;
  existingOnly?: boolean;
} & (
  | { type: "boolean" }
  | { type: "integer"; min: number; max: number }
  | { type: "enum"; options: readonly string[] }
  | { type: "text"; multiline?: boolean }
);

const MAX_INT = 2147483647;
const MIN_INT = -2147483648;

export const PROPERTY_FIELDS: readonly PropertyField[] = [
  { key: "difficulty", group: "game", type: "enum", options: ["peaceful", "easy", "normal", "hard"] },
  { key: "gamemode", group: "game", type: "enum", options: ["survival", "creative", "adventure", "spectator"] },
  { key: "force-gamemode", group: "game", type: "boolean" },
  { key: "hardcore", group: "game", type: "boolean" },
  { key: "allow-flight", group: "game", type: "boolean" },
  { key: "pvp", group: "game", type: "boolean", existingOnly: true },
  { key: "enable-command-block", group: "game", type: "boolean", existingOnly: true },
  { key: "max-players", group: "players", type: "integer", min: 0, max: MAX_INT, managed: "login" },
  { key: "white-list", group: "players", type: "boolean" },
  { key: "enforce-whitelist", group: "players", type: "boolean" },
  { key: "player-idle-timeout", group: "players", type: "integer", min: 0, max: MAX_INT },
  { key: "op-permission-level", group: "players", type: "enum", options: ["0", "1", "2", "3", "4"] },
  { key: "function-permission-level", group: "players", type: "enum", options: ["1", "2", "3", "4"] },
  { key: "hide-online-players", group: "players", type: "boolean" },
  { key: "enforce-secure-profile", group: "players", type: "boolean" },
  { key: "level-name", group: "world", type: "text" },
  { key: "level-seed", group: "world", type: "text" },
  { key: "level-type", group: "world", type: "text" },
  { key: "generate-structures", group: "world", type: "boolean" },
  { key: "spawn-protection", group: "world", type: "integer", min: 0, max: MAX_INT },
  { key: "max-world-size", group: "world", type: "integer", min: 1, max: 29999984 },
  { key: "allow-nether", group: "world", type: "boolean", existingOnly: true },
  { key: "spawn-monsters", group: "world", type: "boolean", existingOnly: true },
  { key: "spawn-animals", group: "world", type: "boolean", existingOnly: true },
  { key: "spawn-npcs", group: "world", type: "boolean", existingOnly: true },
  { key: "motd", group: "network", type: "text", multiline: true },
  { key: "enable-status", group: "network", type: "boolean" },
  { key: "log-ips", group: "network", type: "boolean" },
  { key: "broadcast-console-to-ops", group: "network", type: "boolean" },
  { key: "broadcast-rcon-to-ops", group: "network", type: "boolean" },
  { key: "network-compression-threshold", group: "network", type: "integer", min: MIN_INT, max: MAX_INT },
  { key: "rate-limit", group: "network", type: "integer", min: 0, max: MAX_INT },
  { key: "resource-pack", group: "network", type: "text" },
  { key: "require-resource-pack", group: "network", type: "boolean" },
  { key: "resource-pack-sha1", group: "network", type: "text" },
  { key: "resource-pack-id", group: "network", type: "text" },
  { key: "resource-pack-prompt", group: "network", type: "text", multiline: true },
  { key: "server-port", group: "network", type: "text", managed: "platform" },
  { key: "online-mode", group: "network", type: "text", managed: "platform" },
  { key: "enable-rcon", group: "network", type: "text", managed: "platform" },
  { key: "rcon.port", group: "network", type: "text", managed: "platform" },
  { key: "rcon.password", group: "network", type: "text", managed: "platform" },
  { key: "bungeecord", group: "network", type: "text", managed: "login", loginOnly: true },
  { key: "bungee-guard", group: "network", type: "text", managed: "login", loginOnly: true },
  { key: "velocity-modern", group: "network", type: "text", managed: "login", loginOnly: true },
  { key: "forwarding-secrets", group: "network", type: "text", managed: "login", loginOnly: true },
  { key: "view-distance", group: "performance", type: "integer", min: 3, max: 32 },
  { key: "simulation-distance", group: "performance", type: "integer", min: 3, max: 32 },
  { key: "entity-broadcast-range-percentage", group: "performance", type: "integer", min: 10, max: 1000 },
  { key: "use-native-transport", group: "performance", type: "boolean" },
];

export function propertyManaged(field: PropertyField, server: string): boolean {
  return field.managed === "platform" || (field.managed === "login" && server === "login");
}

export function propertyProblem(field: PropertyField, value: string): "boolean" | "integer" | "enum" | "sha1" | "uuid" | undefined {
  if (field.type === "boolean" && value !== "true" && value !== "false") return "boolean";
  if (field.type === "enum" && !field.options.includes(value)) return "enum";
  if (field.type === "integer" && (!/^[+-]?\d+$/.test(value) || Number(value) < field.min || Number(value) > field.max)) return "integer";
  if (field.key === "resource-pack-sha1" && value !== "" && !/^[\da-f]{40}$/i.test(value)) return "sha1";
  if (field.key === "resource-pack-id" && value !== "" && !/^[\da-f]{8}(-[\da-f]{4}){3}-[\da-f]{12}$/i.test(value)) return "uuid";
}
