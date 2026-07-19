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

describe("session auth wire shapes", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

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

  it("bind POSTs {code} to /auth/bind", async () => {
    const fetchSpy = fakeFetch({
      user_id: "mock-linked",
      linked: true,
      mc_uuid: "uuid-123",
      auth_source: "mojang",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.bind("ABCD2345");
    expect(res.user_id).toBe("mock-linked");
    expect(res.linked).toBe(true);
    expect(res.mc_uuid).toBe("uuid-123");
    expect(res.auth_source).toBe("mojang");

    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/bind");
    expect((opts as RequestInit).method).toBe("POST");
    expect((opts as RequestInit).headers).toEqual({
      "Content-Type": "application/json",
    });
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      code: "ABCD2345",
    });
  });

  it("maps the auth error codes to stable human copy", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ code: "local_auth_disabled" })).toMatch(/turned off/i);
    expect(humanizeError({ code: "staff_account" })).toMatch(/operator/i);
  });

  // Op-login (the staff door): start hands back the approval handle the panel shows
  // as `/felis web op approve <id>`; status is polled; finish spends the mailed code.
  // EXACTLY handlers_op_login.go's request/response keys.
  it("opLoginStart POSTs {email} and surfaces {request_id, expires_at}", async () => {
    const fetchSpy = fakeFetch({
      request_id: "req-1",
      expires_at: "2026-07-19T00:10:00Z",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.opLoginStart("ops@example.test");
    expect(res.request_id).toBe("req-1");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/op-login/start");
    expect((opts as RequestInit).method).toBe("POST");
    expect((opts as RequestInit).headers).toEqual({
      "Content-Type": "application/json",
    });
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      email: "ops@example.test",
    });
  });

  it("opLoginStatus GETs /auth/op-login/status/{id} and surfaces approved", async () => {
    const fetchSpy = fakeFetch({ approved: true });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.opLoginStatus("req-1");
    expect(res.approved).toBe(true);
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/op-login/status/req-1");
    expect((opts as RequestInit).method).toBe("GET");
  });

  it("opLoginFinish POSTs {request_id, code}", async () => {
    const fetchSpy = fakeFetch({ user_id: "u9", role: "admin" });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.opLoginFinish("req-1", "123456");
    expect(res.user_id).toBe("u9");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/auth/op-login/finish");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      request_id: "req-1",
      code: "123456",
    });
  });
});

// Pin the §B3 migration wire shapes (handlers_account_migrate.go). The status union
// ({active:false} | {active:true, state, ...}) and the issue/redeem bodies cross the
// untyped fetch().json() boundary, so a key drift leaves the Account migration card
// inert while typecheck/build stay green.
describe("account migration wire shapes", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("migrateStatus GETs /account/migrate and surfaces the state-machine fields", async () => {
    const fetchSpy = fakeFetch({
      active: true,
      state: "confirmed",
      confirm_factor: "email_otp",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.migrateStatus();
    expect(res.active).toBe(true);
    expect(res.state).toBe("confirmed");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/account/migrate");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
  });

  it("migrateIssueCode POSTs {target_user_id} and surfaces the one-time code", async () => {
    const fetchSpy = fakeFetch({
      code: "MIGR-1234",
      expires_at: "2026-07-19T00:10:00Z",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.migrateIssueCode("u2");
    expect(res.code).toBe("MIGR-1234");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/account/migrate/issue-code");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      target_user_id: "u2",
    });
  });

  it("migrateRedeem POSTs {code} and surfaces the moved servers", async () => {
    const fetchSpy = fakeFetch({ migrated: true, servers_moved: 2, servers: ["a", "b"] });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.migrateRedeem("MIGR-1234");
    expect(res.servers_moved).toBe(2);
    expect(res.servers).toEqual(["a", "b"]);
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/account/migrate/redeem");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      code: "MIGR-1234",
    });
  });
});

// Pin the GET /fleet wire shape (the SysAdmin cockpit's read). It is the ONLY
// place the panel asserts the fleetServerView fields: the cockpit consumes the raw
// CRD names (playersOnline/playersMax, not players/maxPlayers) plus the joined
// `owner`, all crossing the untyped fetch().json() boundary. If the Go handler's
// JSON ever drifts to the /me/servers shape — or drops owner — typecheck/build stay
// green while the table silently renders blank players and "unclaimed" everywhere.
describe("api.fleet wire shape", () => {
  beforeEach(() => vi.clearAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("GETs /fleet and unwraps .servers with the CRD field names + owner", async () => {
    const fetchSpy = fakeFetch({
      servers: [
        {
          name: "survival",
          subdomain: "survival",
          phase: "Running",
          ready: true,
          playersOnline: 3,
          playersMax: 20,
          owner: "alice@example.test",
        },
      ],
    });
    vi.stubGlobal("fetch", fetchSpy);
    const servers = await api.fleet();
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/fleet");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
    expect(servers).toHaveLength(1);
    expect(servers[0].playersOnline).toBe(3);
    expect(servers[0].playersMax).toBe(20);
    expect(servers[0].owner).toBe("alice@example.test");
  });

  it("defaults to [] when the body carries no servers key", async () => {
    const fetchSpy = fakeFetch({});
    vi.stubGlobal("fetch", fetchSpy);
    expect(await api.fleet()).toEqual([]);
  });
});

// Pin the §access wire shapes (handlers_access.go). The panel translates structured
// fields into the request body — the backend re-validates and concatenates the RCON
// command, so the {action, player} keys and the GET-vs-POST split on the same path
// are the contract. A method/path/key drift here is invisible to typecheck (the body
// is `unknown`), so only these assertions catch it.
describe("api access-control wire shapes", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("accessWhitelistList GETs the whitelist path and returns players + raw output", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      players: ["alice", "bob"],
      output: "There are 2 whitelisted player(s): alice, bob",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.accessWhitelistList("survival");
    expect(res.players).toEqual(["alice", "bob"]);
    expect(res.output).toMatch(/alice, bob/);

    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/whitelist");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
  });

  it("accessWhitelist POSTs {action, player} to the same path", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      action: "add",
      player: "alice",
      output: "[ok]",
    });
    vi.stubGlobal("fetch", fetchSpy);
    await api.accessWhitelist("survival", "add", "alice");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/whitelist");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      action: "add",
      player: "alice",
    });
  });

  it("accessBan POSTs {action, player} to the ban path (no reason field)", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      action: "ban",
      player: "griefer",
      output: "[ok]",
    });
    vi.stubGlobal("fetch", fetchSpy);
    await api.accessBan("survival", "ban", "griefer");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/ban");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      action: "ban",
      player: "griefer",
    });
  });

  it("accessBanList GETs the ban path and returns players + raw output", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      players: ["griefer", "spammer"],
      output:
        "There are 2 ban(s):\ngriefer was banned by Server: x\nspammer was banned by Server: y",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.accessBanList("survival");
    expect(res.players).toEqual(["griefer", "spammer"]);
    expect(res.output).toMatch(/griefer was banned by/);

    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/ban");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
  });

  it("accessPlayers GETs the players path and returns tally + names + raw output", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      online: 2,
      max: 20,
      players: ["alice", "bob"],
      output: "There are 2 of a max of 20 players online: alice, bob",
    });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.accessPlayers("survival");
    expect(res.online).toBe(2);
    expect(res.max).toBe(20);
    expect(res.players).toEqual(["alice", "bob"]);

    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/players");
    expect((opts as RequestInit).method).toBe("GET");
    expect((opts as RequestInit).credentials).toBe("include");
  });

  it("accessKick POSTs {player} to the kick path (no action, no reason)", async () => {
    const fetchSpy = fakeFetch({
      name: "survival",
      player: "griefer",
      output: "[ok]",
    });
    vi.stubGlobal("fetch", fetchSpy);
    await api.accessKick("survival", "griefer");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock
      .calls[0];
    expect(String(url)).toBe("/servers/survival/access/kick");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      player: "griefer",
    });
  });

  it("maps the access error codes to stable human copy", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ code: "not_running" })).toMatch(/running|wake/i);
    expect(humanizeError({ code: "console_unavailable" })).toMatch(/console/i);
  });
});

describe("image whitelist and builds wire shapes", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("addImage POSTs {image_ref} to /images", async () => {
    const fetchSpy = fakeFetch({ image_ref: "x", source: "external", enabled: true });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.addImage("x");
    expect(res.image_ref).toBe("x");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({ image_ref: "x" });
  });

  it("removeImage DELETEs with ref in query params to /images", async () => {
    const fetchSpy = fakeFetch(null);
    vi.stubGlobal("fetch", fetchSpy);
    await api.removeImage("x");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images?ref=x");
    expect((opts as RequestInit).method).toBe("DELETE");
  });

  it("buildImage POSTs build details to /images/build", async () => {
    const fetchSpy = fakeFetch({ id: "bld-1", image_ref: "x", status: "pending" });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.buildImage({ image_ref: "x", dockerfile: "FROM x", context_ref: "c" });
    expect(res.id).toBe("bld-1");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images/build");
    expect((opts as RequestInit).method).toBe("POST");
    expect(JSON.parse((opts as RequestInit).body as string)).toEqual({
      image_ref: "x",
      dockerfile: "FROM x",
      context_ref: "c",
    });
  });

  it("getBuild GETs build status from /images/build/{id}", async () => {
    const fetchSpy = fakeFetch({ id: "bld-1", image_ref: "x", status: "building" });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.getBuild("bld-1");
    expect(res.status).toBe("building");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images/build/bld-1");
    expect((opts as RequestInit).method).toBe("GET");
  });

  it("cancelBuild POSTs to cancel endpoint /images/build/{id}/cancel", async () => {
    const fetchSpy = fakeFetch({ id: "bld-1", image_ref: "x", status: "cancelled" });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.cancelBuild("bld-1");
    expect(res.status).toBe("cancelled");
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images/build/bld-1/cancel");
    expect((opts as RequestInit).method).toBe("POST");
  });

  describe("submissions", () => {
    it("listSubmissions GETs from /submissions", async () => {
      const submissions = [{ id: "sub-1", display_name: "test", status: "pending_review" }];
      const fetchSpy = fakeFetch({ submissions });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listSubmissions();
      expect(res).toEqual(submissions);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("approveSubmission POSTs to /submissions/{id}/approve", async () => {
      const sub = { id: "sub-1", status: "approved" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.approveSubmission("sub-1");
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions/sub-1/approve");
      expect((opts as RequestInit).method).toBe("POST");
    });

    it("rejectSubmission POSTs {reason} to /submissions/{id}/reject", async () => {
      const sub = { id: "sub-1", status: "rejected", reject_reason: "bad" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.rejectSubmission("sub-1", "bad");
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions/sub-1/reject");
      expect((opts as RequestInit).method).toBe("POST");
      expect(JSON.parse((opts as RequestInit).body as string)).toEqual({ reason: "bad" });
    });

    it("listMySubmissions GETs from /me/submissions", async () => {
      const submissions = [{ id: "sub-2", display_name: "my test", status: "pending_review" }];
      const fetchSpy = fakeFetch({ submissions });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listMySubmissions();
      expect(res).toEqual(submissions);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/me/submissions");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("createSubmission POSTs {display_name} to /me/submissions", async () => {
      const sub = { id: "sub-3", display_name: "new submission", status: "pending_review" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.createSubmission("new submission");
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/me/submissions");
      expect((opts as RequestInit).method).toBe("POST");
      expect(JSON.parse((opts as RequestInit).body as string)).toEqual({ display_name: "new submission" });
    });

    it("uploadSubmissionContext POSTs Blob to /me/submissions/{id}/context", async () => {
      const sub = { id: "sub-3", display_name: "new submission", status: "pending_review" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const blob = new Blob(["test"], { type: "application/x-gzip" });
      const res = await api.uploadSubmissionContext("sub-3", blob);
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/me/submissions/sub-3/context");
      expect((opts as RequestInit).method).toBe("POST");
      expect((opts as RequestInit).body).toBe(blob);
      expect((opts as RequestInit).headers).toEqual({ "Content-Type": "application/x-gzip" });
    });
  });

  describe("updates maintenance window", () => {
    it("getUpdateWindow GETs from /updates/window", async () => {
      const win = { start: "2026-07-03T12:00:00Z", end: "2026-07-03T14:00:00Z" };
      const fetchSpy = fakeFetch(win);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.getUpdateWindow();
      expect(res).toEqual(win);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/updates/window");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("setUpdateWindow PUTs UpdateWindow payload to /updates/window", async () => {
      const win = { start: "2026-07-03T12:00:00Z", end: "2026-07-03T14:00:00Z" };
      const fetchSpy = fakeFetch(win);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.setUpdateWindow(win);
      expect(res).toEqual(win);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/updates/window");
      expect((opts as RequestInit).method).toBe("PUT");
      expect((opts as RequestInit).headers).toEqual({ "Content-Type": "application/json" });
      expect(JSON.parse((opts as RequestInit).body as string)).toEqual(win);
    });
  });
});
