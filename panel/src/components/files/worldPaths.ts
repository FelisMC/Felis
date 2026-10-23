import { getProperty } from "./properties";

function clean(path: string): string {
  const parts: string[] = [];
  for (const part of path.split("/")) {
    if (part === "..") parts.pop();
    else if (part !== "" && part !== ".") parts.push(part);
  }
  return parts.join("/");
}

export function worldDirectories(properties: string): string[] {
  const level = getProperty(properties, "level-name") ?? "world";
  return [...new Set(["world", "world_nether", "world_the_end", level, `${level}_nether`, `${level}_the_end`].map(clean))];
}

export function touchesWorld(path: string, directories: readonly string[]): boolean {
  const target = clean(path);
  return directories.some((dir) => dir === "" || target === "" || target === dir || target.startsWith(`${dir}/`) || dir.startsWith(`${target}/`));
}
