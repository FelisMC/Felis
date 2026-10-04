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
    { key: "bookAction", value: "▶ 点此打开登录页\n▶ Open login page", multiline: true },
    { key: "bookHelp", value: "在系统浏览器中完成。\nUse your SYSTEM browser —\nnot WeChat / QQ (passkey\nwon't work there).", multiline: true },
    { key: "openBook", value: true },
    { key: "loginTimeoutSeconds", value: 600, min: 30, max: 3600 },
  ] },
];

export function experienceValid(values: Experience, groups: ExperienceGroup[]): boolean {
  return groups.flatMap((g) => g.fields).every((field) => {
    const value = values[field.key] ?? field.value;
    if (typeof value !== typeof field.value) return false;
    if (typeof value === "number") return Number.isInteger(value) && value >= field.min! && value <= field.max!;
    if (typeof value === "string") return value.length <= 512 && (!field.choices || field.choices.includes(value));
    return true;
  });
}

export async function readExperience(name: string, groups: ExperienceGroup[]): Promise<{ values: Experience; sha256: string }> {
  let file;
  try {
    file = await api.readServerFile(name, EXPERIENCE_PATH);
  } catch (error) {
    if ((error as ApiError)?.code === "not_found") return { values: {}, sha256: "" };
    throw error;
  }
  const bytes = Uint8Array.from(atob(file.content), (char) => char.charCodeAt(0));
  const values: unknown = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  if (!values || typeof values !== "object" || Array.isArray(values) || !experienceValid(values as Experience, groups)) {
    throw new Error("invalid_experience");
  }
  return { values: values as Experience, sha256: file.sha256 };
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
