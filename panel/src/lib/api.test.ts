import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// Pin the GET /me wire shape. is_admin crosses an untyped fetch().json() boundary
// (request() does `return parsed as T`), so nothing but a test pins the snake_case
// keys to the Go handler. If the handler's JSON ever drifts to camelCase — or the
// client starts transforming keys — `identity.is_admin` becomes undefined, which is
// falsy, and EVERY admin silently renders as a non-admin while typecheck/build stay
// green. This fixture is the canonical mirror of handlers_user.go handleMe.

vi.mock("./config", () => ({
  loadConfig: async () => ({ apiBase: "", rootDomain: "example.test" }),
}));

// Imported after the mock so api.ts picks up the mocked loadConfig.
const { api } = await import("./api");

function fakeFetch(body: unknown, init?: { ok?: boolean; status?: number }) {
  return vi.fn(async () => ({
    ok: init?.ok ?? true,
    status: init?.status ?? 200,
    statusText: "OK",
    text: async () => JSON.stringify(body),
  })) as unknown as typeof fetch;
}

describe("api.me wire shape", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("surfaces snake_case is_admin from GET /me verbatim (admin)", async () => {
    // EXACTLY the JSON handlers_user.go handleMe emits.
    const body = { user_id: "u1", email: "a@b.c", role: "admin", is_admin: true };
    vi.stubGlobal("fetch", fakeFetch(body));
    const id = await api.me();
    expect(id.is_admin).toBe(true);
    expect(id.user_id).toBe("u1");
    expect(id.email).toBe("a@b.c");
    expect(id.role).toBe("admin");
  });

  it("reports is_admin:false for a non-admin identity", async () => {
    const body = { user_id: "u2", email: "x@y.z", role: "user", is_admin: false };
    vi.stubGlobal("fetch", fakeFetch(body));
    const id = await api.me();
    expect(id.is_admin).toBe(false);
    expect(id.role).toBe("user");
  });

  it("calls GET on the /me path", async () => {
    const fetchSpy = fakeFetch({
      user_id: "u3",
      email: "m@n.o",
      role: "user",
      is_admin: false,
    });
    vi.stubGlobal("fetch", fetchSpy);
    await api.me();
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/me");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
  });
});
