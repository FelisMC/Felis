import { beforeEach } from "vitest";

// Install in-memory storage before i18n detects availability; Node's global storage can shadow jsdom's.
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
