import { api } from "./api";
import type { ApiError } from "./types";

export const EXPERIENCE_PATH = "felis-experience.json";
export type Experience = Record<string, unknown>;
export type ExperienceField = {
  key: string;
  value: string | number | boolean;
  choices?: string[];
  min?: number;
  max?: number;
  multiline?: boolean;
};
export type ExperienceGroup = { key: string; fields: ExperienceField[] };

export const LOBBY_GROUPS: ExperienceGroup[] = [
  { key: "appearance", fields: [
    { key: "menuTitleZh", value: "Felis 服务器" },
    { key: "menuTitleEn", value: "Felis Servers" },
    { key: "welcomeZh", value: "欢迎来到大厅。输入 /menu 或点击 " },
    { key: "welcomeEn", value: "Welcome to the lobby. Type /menu or click " },
    { key: "showWelcome", value: true },
    { key: "openMenuOnJoin", value: false },
  ] },
  { key: "players", fields: [
    { key: "gameMode", value: "ADVENTURE", choices: ["ADVENTURE", "SURVIVAL", "CREATIVE", "SPECTATOR"] },
    { key: "teleportOnJoin", value: true },
    { key: "protectBuild", value: true },
    { key: "invulnerable", value: true },
    { key: "disableHunger", value: true },
    { key: "voidRescue", value: true },
  ] },
  { key: "world", fields: [
    { key: "difficulty", value: "PEACEFUL", choices: ["PEACEFUL", "EASY", "NORMAL", "HARD"] },
    { key: "time", value: 6000, min: 0, max: 23999 },
    { key: "freezeTime", value: true },
    { key: "clearWeather", value: true },
    { key: "pvp", value: false },
    { key: "spawnMobs", value: false },
    { key: "mobGriefing", value: false },
    { key: "tntExplodes", value: false },
    { key: "keepInventory", value: true },
    { key: "immediateRespawn", value: true },
    { key: "showAdvancementMessages", value: false },
  ] },
];

export const LOGIN_GROUPS: ExperienceGroup[] = [
  { key: "loginBook", fields: [
    { key: "bookTitle", value: "Felis Login" },
    { key: "bookAuthor", value: "Felis" },
    { key: "bookHeading", value: "Felis 登录 / Login" },
    { key: "loginTimeoutSeconds", value: 600, min: 30, max: 3600 },
    { key: "bookAction", value: "▶ 点此打开登录页\n▶ Open login page", multiline: true },
    { key: "bookHelp", value: "在系统浏览器中完成。\nUse your SYSTEM browser —\nnot WeChat / QQ (passkey\nwon't work there).", multiline: true },
    { key: "openBook", value: true },
  ] },
];

export function experienceValid(values: Experience, groups: ExperienceGroup[]): boolean {
  return groups.flatMap((g) => g.fields).every((field) => fieldValid(field, values[field.key] ?? field.value));
}

function fieldValid(field: ExperienceGroup["fields"][number], value: unknown): boolean {
  if (typeof value !== typeof field.value) return false;
  if (typeof value === "number") return Number.isInteger(value) && value >= field.min! && value <= field.max!;
  if (typeof value === "string") return value.length <= 512 && (!field.choices || field.choices.includes(value));
  return true;
}

// salvage keeps the settings of a damaged file that are still valid; the rest falls back to
// its default, which is also what the game runs on.
function salvage(raw: unknown, groups: ExperienceGroup[]): Experience {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
  const kept: Experience = {};
  for (const field of groups.flatMap((g) => g.fields)) {
    const value = (raw as Experience)[field.key];
    if (value !== undefined && fieldValid(field, value)) kept[field.key] = value;
  }
  return kept;
}

// readExperience reads the settings file. A file damaged by hand (in the file manager) is
// not an error: the game runs on the defaults, and the page shows what it can salvage with
// `damaged` set, so saving writes a good file over it.
export async function readExperience(name: string, groups: ExperienceGroup[]): Promise<{ values: Experience; sha256: string; damaged: boolean }> {
  let file;
  try {
    file = await api.readServerFile(name, EXPERIENCE_PATH);
  } catch (error) {
    if ((error as ApiError)?.code === "not_found") return { values: {}, sha256: "", damaged: false };
    throw error;
  }
  let values: unknown;
  try {
    const bytes = Uint8Array.from(atob(file.content), (char) => char.charCodeAt(0));
    values = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch {
    return { values: {}, sha256: file.sha256, damaged: true };
  }
  if (!values || typeof values !== "object" || Array.isArray(values) || !experienceValid(values as Experience, groups)) {
    return { values: salvage(values, groups), sha256: file.sha256, damaged: true };
  }
  return { values: values as Experience, sha256: file.sha256, damaged: false };
}

export async function writeExperience(name: string, values: Experience, sha256: string): Promise<string> {
  const bytes = new TextEncoder().encode(JSON.stringify(values, null, 2) + "\n");
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  const content = btoa(binary);
  const file = sha256
    ? await api.writeServerFile(name, EXPERIENCE_PATH, content, sha256)
    : await api.createServerFile(name, EXPERIENCE_PATH, content);
  return file.sha256;
}
