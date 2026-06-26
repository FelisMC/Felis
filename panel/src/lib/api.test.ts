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

  it("surfaces must_change_password from GET /me verbatim", async () => {
    // handleMe always emits must_change_password; the forced-change gate routes on
    // it, so the snake_case key must survive the untyped boundary unchanged.
    const body = {
      user_id: "u4",
      email: "o@p.q",
      role: "admin",
      is_admin: true,
      must_change_password: true,
    };
    vi.stubGlobal("fetch", fakeFetch(body));
    const id = await api.me();
    expect(id.must_change_password).toBe(true);
  });
});

describe("local-password auth wire shapes", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("login POSTs {username, password} and returns must_change_password", async () => {
    // EXACTLY handlers_auth.go handleLogin's request body and response.
    const fetchSpy = fakeFetch({
      user_id: "u1",
      role: "admin",
      must_change_password: true,
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.login("owner", "s3cret");
    expect(res.must_change_password).toBe(true);
    expect(res.user_id).toBe("u1");

    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/login");
    expect((opts as RequestInit).method).toBe("POST");
    expect((opts as RequestInit).credentials).toBe("include");
    // The Go login route now REQUIRES Content-Type: application/json (it 415s any
    // other type to kill the cross-site form-POST forgery vector). This pins the
    // panel half of that contract: a refactor that drops the header silently breaks
    // login, and only this assertion would catch it.
    expect((opts as RequestInit).headers).toEqual({
      "Content-Type": "application/json",
    });
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      username: "owner",
      password: "s3cret",
    });
  });

  it("logout POSTs to /auth/logout (idempotent {ok:true})", async () => {
    const fetchSpy = fakeFetch({ ok: true });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.logout();
    expect(res.ok).toBe(true);
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/logout");
    expect((opts as RequestInit).method).toBe("POST");
  });

  it("changePassword POSTs {current_password, new_password}", async () => {
    const fetchSpy = fakeFetch({ ok: true });
    vi.stubGlobal("fetch", fetchSpy);
    await api.changePassword("old-pw", "brand-new-pw");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/change-password");
    expect((opts as RequestInit).method).toBe("POST");
    // Same JSON content-type contract as login — the change-password route guards on
    // it too (defense-in-depth), so the panel must keep sending it.
    expect((opts as RequestInit).headers).toEqual({
      "Content-Type": "application/json",
    });
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      current_password: "old-pw",
      new_password: "brand-new-pw",
    });
  });

  it("maps the auth error codes to stable human copy", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ code: "invalid_credentials" })).toMatch(/incorrect/i);
    expect(humanizeError({ code: "local_auth_disabled" })).toMatch(/turned off/i);
    expect(humanizeError({ code: "weak_password" })).toMatch(/8 and 72/);
    expect(humanizeError({ code: "password_unchanged" })).toMatch(/differ/i);
  });
});
