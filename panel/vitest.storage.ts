import { beforeEach } from "vitest";

// Node 26 puts its own localStorage on globalThis, undefined unless node runs
// with --localstorage-file, and it shadows jsdom's. Component tests get an
// in-memory one so code that remembers things (the language, console history)
// runs as in a browser. This file runs before the i18n boot in vitest.setup.ts,
// because the language detector decides once whether storage works.
class MemoryStorage implements Storage {
  #data = new Map<string, string>();
  get length() {
    return this.#data.size;
  }
  key(index: number) {
    return [...this.#data.keys()][index] ?? null;
  }
  getItem(key: string) {
    return this.#data.get(key) ?? null;
  }
  setItem(key: string, value: string) {
    this.#data.set(key, String(value));
  }
  removeItem(key: string) {
    this.#data.delete(key);
  }
  clear() {
    this.#data.clear();
  }
}

if (typeof window !== "undefined" && !globalThis.localStorage) {
  Object.defineProperty(globalThis, "localStorage", {
    value: new MemoryStorage(),
    configurable: true,
    writable: true,
  });
  // Each test starts empty, including of the boot's own cached language.
  beforeEach(() => localStorage.clear());
}
