function clean(path: string): string {
  const parts: string[] = [];
  for (const part of path.split("/")) {
    if (part === "..") parts.pop();
    else if (part !== "" && part !== ".") parts.push(part);
  }
  return parts.join("/");
}

export function worldDirectories(properties: string): string[] {
  let level = "world";
  const logical = properties.replace(/((?:^|[^\\])(?:\\\\)*)\\\r?\n[ \t\f]*/g, "$1");
  for (const line of logical.split(/\r?\n/)) {
    const match = /^\s*level-name(?:\s*[:=]\s*|\s+)(.*)$/.exec(line);
    if (match) {
      level = match[1].replace(/\\u([\da-fA-F]{4})|\\(.)/g, (_, hex: string | undefined, char: string) =>
        hex ? String.fromCharCode(parseInt(hex, 16)) : ({ t: "\t", n: "\n", r: "\r", f: "\f" }[char] ?? char),
      );
    }
  }
  return [...new Set(["world", "world_nether", "world_the_end", level, `${level}_nether`, `${level}_the_end`].map(clean))];
}

export function touchesWorld(path: string, directories: readonly string[]): boolean {
  const target = clean(path);
  return directories.some((dir) => dir === "" || target === "" || target === dir || target.startsWith(`${dir}/`) || dir.startsWith(`${target}/`));
}
