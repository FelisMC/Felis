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
const { api, SETUP_REQUIRED_EVENT, SESSION_EXPIRED_EVENT, CONNECTION_EVENT, humanizeError, isConnectionLost, clientError } =
  await import("./api");

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

  it("says a server name is held by a world volume left behind", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ status: 409, code: "world_volume_exists" })).toBe(
      "An earlier server with that name left its world volume behind. Choose another name, or ask the operator to delete the old volume.",
    );
  });

  it("says why a user change was refused for the caller's own or the owner account", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ status: 403, code: "self_protected" })).toBe(
      "You can't do that to the account you're signed in with.",
    );
    expect(humanizeError({ status: 403, code: "owner_protected" })).toBe(
      "The owner account can't be demoted, disabled or deleted from the panel. Only the host's break-glass console (sudo felis breakGlass) manages it.",
    );
  });

  it("says email codes are off when the install has no mail relay", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ status: 503, code: "mail_unavailable" })).toBe(
      "This server can't send email codes because no mail relay is set up. Sign in with a passkey, or ask the server operator to configure email.",
    );
  });

  it("maps the backup rationing codes to their own copy", async () => {
    const { humanizeError } = await import("./api");
    expect(humanizeError({ code: "backup_cooldown" })).toMatch(/cooldown/i);
    expect(humanizeError({ code: "backup_store_full" })).toMatch(/store is full/i);
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

  it("listBuilds GETs one page of /images/build with the query", async () => {
    const fetchSpy = fakeFetch({ builds: [{ id: "bld-2", image_ref: "x", status: "building" }], total: 41 });
    vi.stubGlobal("fetch", fetchSpy);
    const res = await api.listBuilds({ query: "paper 1.21", limit: 20, offset: 40 });
    expect(res.total).toBe(41);
    expect(res.builds.map((b) => b.id)).toEqual(["bld-2"]);
    const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/images/build?query=paper+1.21&limit=20&offset=40");
    expect((opts as RequestInit).method).toBe("GET");
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
    it("listSubmissions GETs a page from /submissions", async () => {
      const submissions = [{ id: "sub-1", display_name: "test", status: "pending_review" }];
      const fetchSpy = fakeFetch({ submissions, total: 31, counts: { pending_review: 4, approved: 27, rejected: 0 } });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listSubmissions();
      expect(res).toEqual({ submissions, total: 31, counts: { pending_review: 4, approved: 27, rejected: 0 } });
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("listSubmissions puts the filter and page on the query string", async () => {
      const fetchSpy = fakeFetch({ submissions: [], total: 0, counts: {} });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listSubmissions({ status: "approved", query: "sky block", limit: 10, offset: 20 });
      expect(res).toEqual({ submissions: [], total: 0, counts: { pending_review: 0, approved: 0, rejected: 0 } });
      const [url] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions?status=approved&query=sky+block&limit=10&offset=20");
    });

    it("approveSubmission POSTs to /submissions/{id}/approve", async () => {
      const sub = { id: "sub-1", status: "approved" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const digest = "a".repeat(64);
      const res = await api.approveSubmission("sub-1", digest);
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions/sub-1/approve");
      expect((opts as RequestInit).method).toBe("POST");
      expect(JSON.parse((opts as RequestInit).body as string)).toEqual({ expected_digest: digest });
    });

    it("downloadSubmissionContext resolves to the digest the API streamed", async () => {
      const digest = "b".repeat(64);
      const fetchSpy = vi.fn(async () => ({
        ok: true,
        status: 200,
        statusText: "OK",
        headers: new Headers({ "X-Felis-Context-Sha256": digest.toUpperCase() }),
        blob: async () => new Blob(["ctx"]),
      })) as unknown as typeof fetch;
      vi.stubGlobal("fetch", fetchSpy);
      vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:ctx");
      vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
      const link = { href: "", download: "", click: vi.fn() };
      vi.stubGlobal("document", { createElement: () => link });
      await expect(api.downloadSubmissionContext("sub-1")).resolves.toBe(digest);
      expect(link.click).toHaveBeenCalledOnce();
      expect(link.download).toBe("sub-1-context.tar.gz");
      const [url] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions/sub-1/context");
    });

    it("getBuildScan GETs the kept scan, and downloadBuildScanDocument saves each document under its own name", async () => {
      const fetchSpy = fakeFetch({ build_id: "bld-7", has_report: true, has_sbom: true });
      vi.stubGlobal("fetch", fetchSpy);
      await api.getBuildScan("bld-7");
      expect(String((fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0])).toBe("/images/build/bld-7/scan");

      const docFetch = vi.fn(async () => ({
        ok: true,
        status: 200,
        statusText: "OK",
        headers: new Headers(),
        blob: async () => new Blob(["{}"]),
      })) as unknown as typeof fetch;
      vi.stubGlobal("fetch", docFetch);
      vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:doc");
      vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
      const links: { href: string; download: string; click: () => void }[] = [];
      vi.stubGlobal("document", {
        createElement: () => {
          const link = { href: "", download: "", click: vi.fn() };
          links.push(link);
          return link;
        },
      });
      await api.downloadBuildScanDocument("bld-7", "report");
      await api.downloadBuildScanDocument("bld-7", "sbom");
      const urls = (docFetch as unknown as ReturnType<typeof vi.fn>).mock.calls.map(([u]) => String(u));
      expect(urls).toEqual(["/images/build/bld-7/scan/report", "/images/build/bld-7/sbom"]);
      expect(links.map((l) => l.download)).toEqual(["bld-7-trivy.json", "bld-7.cdx.json"]);
      expect(links.every((l) => (l.click as ReturnType<typeof vi.fn>).mock.calls.length === 1)).toBe(true);
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

    it("listMySubmissions GETs a page from /me/submissions", async () => {
      const submissions = [{ id: "sub-2", display_name: "my test", status: "pending_review" }];
      const fetchSpy = fakeFetch({ submissions, total: 1, counts: { pending_review: 1, approved: 0, rejected: 2 } });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listMySubmissions({ status: "rejected", offset: 10 });
      expect(res).toEqual({ submissions, total: 1, counts: { pending_review: 1, approved: 0, rejected: 2 } });
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/me/submissions?status=rejected&offset=10");
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

    // The lane's two throttled outcomes (a spent allowance, a closed cooldown)
    // must surface as their own copy, not the generic forbidden/error text.
    it("maps the submission quota/cooldown codes to stable human copy", async () => {
      const { humanizeError } = await import("./api");
      expect(humanizeError({ code: "submission_quota_exceeded" })).toMatch(/quota/i);
      expect(humanizeError({ code: "submission_cooldown" })).toMatch(/try again/i);
    });

    it("withdrawSubmission DELETEs /me/submissions/{id}", async () => {
      const sub = { id: "sub-4", status: "pending_review" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.withdrawSubmission("sub-4");
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/me/submissions/sub-4");
      expect((opts as RequestInit).method).toBe("DELETE");
    });

    it("deleteSubmission DELETEs /submissions/{id}", async () => {
      const sub = { id: "sub-5", status: "rejected" };
      const fetchSpy = fakeFetch(sub);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.deleteSubmission("sub-5");
      expect(res).toEqual(sub);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/submissions/sub-5");
      expect((opts as RequestInit).method).toBe("DELETE");
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

  describe("control-plane database backup", () => {
    it("getDBBackup GETs /platform/db-backup and keeps a null last", async () => {
      const status = { last: null, stale: true, max_age_seconds: 93600 };
      const fetchSpy = fakeFetch(status);
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.getDBBackup();
      expect(res).toEqual(status);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/platform/db-backup");
      expect((opts as RequestInit).method).toBe("GET");
    });
  });

  describe("backup now and server jobs wire shapes", () => {
    it("backupNow POSTs to /servers/{name}/backup with no body and parses the 202", async () => {
      const fetchSpy = fakeFetch({ name: "survival", status: "backing_up" }, { status: 202 });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.backupNow("survival");
      expect(res.status).toBe("backing_up");
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/servers/survival/backup");
      expect((opts as RequestInit).method).toBe("POST");
      expect((opts as RequestInit).body).toBeUndefined();
    });

    it("listBackups asks for one server's page and keeps the total", async () => {
      const backup = { id: "bk1", server_name: "survival", status: "present", created_at: "2026-09-01T00:00:00Z" };
      const fetchSpy = fakeFetch({ backups: [backup], total: 45 });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listBackups({ server: "survival", limit: 20, offset: 40 });
      expect(res).toEqual({ backups: [backup], total: 45 });
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/backups?server=survival&limit=20&offset=40");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("listBackups with no filter reads /backups and fills an empty page", async () => {
      const fetchSpy = fakeFetch({});
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listBackups();
      expect(res).toEqual({ backups: [], total: 0 });
      const [url] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/backups");
    });

    it("restoreBackup sends backup_id, and safety_snapshot only when turned off", async () => {
      const fetchSpy = fakeFetch(
        { name: "survival", status: "restoring", backup_id: "bk1", safety_snapshot: true },
        { status: 202 },
      );
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.restoreBackup("survival", "bk1");
      expect(res.safety_snapshot).toBe(true);
      const calls = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls;
      expect(String(calls[0][0])).toBe("/servers/survival/restore-backup");
      expect(JSON.parse((calls[0][1] as RequestInit).body as string)).toEqual({ backup_id: "bk1" });

      await api.restoreBackup("survival", "bk1", false);
      expect(JSON.parse((calls[1][1] as RequestInit).body as string)).toEqual({
        backup_id: "bk1",
        safety_snapshot: false,
      });

      await api.restoreBackup("survival");
      expect((calls[2][1] as RequestInit).body).toBeUndefined();
    });

    it("serverJobs GETs /servers/{name}/jobs and unwraps the jobs array", async () => {
      const job = {
        name: "felis-backup-survival-123",
        kind: "backup",
        state: "failed",
        message: "pod terminated",
        started_at: "2026-07-03T12:00:00Z",
      };
      const fetchSpy = fakeFetch({ server: "survival", jobs: [job] });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.serverJobs("survival");
      expect(res).toHaveLength(1);
      expect(res[0].state).toBe("failed");
      expect(res[0].message).toBe("pod terminated");
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/servers/survival/jobs");
      expect((opts as RequestInit).method).toBe("GET");
    });
  });

  describe("server file editor wire shape", () => {
    it("listServerFiles GETs /servers/{name}/files with the path as a query parameter", async () => {
      const entries = [
        { name: "world", size: 0, is_dir: true, mod_time: "2026-07-03T12:00:00Z" },
        { name: "server.properties", size: 580, is_dir: false, mod_time: "2026-07-03T12:00:00Z" },
      ];
      const fetchSpy = fakeFetch({ path: "config", entries, truncated: false });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.listServerFiles("survival", "config/old stuff");
      expect(res.entries).toHaveLength(2);
      expect(res.entries[1].is_dir).toBe(false);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      // "/" and " " must survive the query encoding — the path is a query value,
      // never a path segment.
      expect(String(url)).toBe("/servers/survival/files?path=config%2Fold%20stuff");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("readServerFile GETs /servers/{name}/file and passes base64 through", async () => {
      const fetchSpy = fakeFetch({ path: "world/level.dat", content: "AAEC" });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.readServerFile("survival", "world/level.dat");
      expect(res.content).toBe("AAEC");
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/servers/survival/file?path=world%2Flevel.dat");
      expect((opts as RequestInit).method).toBe("GET");
    });

    it("writeServerFile PUTs {content} — an explicit \"\" is a deliberate truncate, not an omitted field", async () => {
      const fetchSpy = fakeFetch({ path: "a.txt", status: "written" });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.writeServerFile("survival", "a.txt", "");
      expect(res.status).toBe("written");
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/servers/survival/file?path=a.txt");
      expect((opts as RequestInit).method).toBe("PUT");
      expect((opts as RequestInit).body).toBe(JSON.stringify({ content: "" }));
    });

    it("writeServerFile sends the hash the read returned as expect_sha256", async () => {
      const fetchSpy = fakeFetch({ path: "a.txt", status: "written", sha256: "b".repeat(64) });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.writeServerFile("survival", "a.txt", "aGk=", "a".repeat(64));
      expect(res.sha256).toBe("b".repeat(64));
      const [, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect((opts as RequestInit).body).toBe(
        JSON.stringify({ content: "aGk=", expect_sha256: "a".repeat(64) }),
      );
    });
  });

  describe("user passkey unbind wire shape", () => {
    it("unbindUserPasskeys DELETEs /users/{id}/passkeys", async () => {
      const fetchSpy = fakeFetch({ ok: true });
      vi.stubGlobal("fetch", fetchSpy);
      const res = await api.unbindUserPasskeys("u1");
      expect(res.ok).toBe(true);
      const [url, opts] = (fetchSpy as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
      expect(String(url)).toBe("/users/u1/passkeys");
      expect((opts as RequestInit).method).toBe("DELETE");
    });
  });

  // #8: a locked session gets `403 setup_required` on every protected route. The
  // panel must not render that as a permission error — it announces the code so
  // App.tsx can route the person to /setup. These tests pin the announcement
  // (fire on the exact code, stay silent on any other 403) because a regression
  // here reappears as the confusing "无权执行此操作" report, with nothing failing.
  describe("setup_required routing signal", () => {
    it("emits SETUP_REQUIRED_EVENT for a 403 setup_required", async () => {
      const target = new EventTarget();
      vi.stubGlobal("window", target);
      let hits = 0;
      target.addEventListener(SETUP_REQUIRED_EVENT, () => {
        hits += 1;
      });
      vi.stubGlobal(
        "fetch",
        fakeFetch(
          {
            error: {
              code: "setup_required",
              message: "passkey enrollment is required before this action is available",
            },
          },
          { ok: false, status: 403 },
        ),
      );
      await expect(api.me()).rejects.toMatchObject({ status: 403, code: "setup_required" });
      expect(hits).toBe(1);
    });

    it("stays silent for an ordinary 403", async () => {
      const target = new EventTarget();
      vi.stubGlobal("window", target);
      let hits = 0;
      target.addEventListener(SETUP_REQUIRED_EVENT, () => {
        hits += 1;
      });
      vi.stubGlobal(
        "fetch",
        fakeFetch({ error: { code: "forbidden", message: "no" } }, { ok: false, status: 403 }),
      );
      await expect(api.me()).rejects.toMatchObject({ status: 403, code: "forbidden" });
      expect(hits).toBe(0);
    });
  });
});

describe("path parameters", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  function lastURL(spy: typeof fetch): string {
    const calls = (spy as unknown as ReturnType<typeof vi.fn>).mock.calls;
    return String(calls[calls.length - 1][0]);
  }

  it('encodes "/", "?" and "#" inside one segment', async () => {
    const spy = fakeFetch({ player: "x", groups: [], permissions: [], output: "" });
    vi.stubGlobal("fetch", spy);
    await api.accessLuckPermsInfo("lobby", "a/b?c#d");
    expect(lastURL(spy)).toBe("/servers/lobby/access/luckperms/a%2Fb%3Fc%23d");
  });

  it("keeps a decoded router param that climbs the path inside its segment", async () => {
    // react-router decodes "..%2F..%2Fusers%2Fu1%3F" to this before a page sees it.
    const spy = fakeFetch({ name: "x", desiredState: "Stopped" });
    vi.stubGlobal("fetch", spy);
    await api.stop("../../users/u1?");
    expect(lastURL(spy)).toBe("/servers/..%2F..%2Fusers%2Fu1%3F/stop");
  });

  it("refuses dot segments and empty ones before sending anything", async () => {
    const spy = fakeFetch({});
    vi.stubGlobal("fetch", spy);
    for (const bad of ["..", ".", ""]) {
      await expect(api.stop(bad)).rejects.toMatchObject({ status: 0, code: "bad_path_param" });
      await expect(api.deleteUser(bad)).rejects.toMatchObject({ code: "bad_path_param" });
    }
    expect(spy).not.toHaveBeenCalled();
    expect(humanizeError({ status: 0, code: "bad_path_param" })).toMatch(/not valid/i);
  });

  it("does not encode a value twice", async () => {
    const spy = fakeFetch({ ok: true });
    vi.stubGlobal("fetch", spy);
    await api.revokeUserSession("u1", "ab/cd");
    expect(lastURL(spy)).toBe("/users/u1/sessions/ab%2Fcd");
    await api.listServerFiles("lobby", "world/level.dat");
    expect(lastURL(spy)).toBe("/servers/lobby/files?path=world%2Flevel.dat");
  });
});

describe("the caller's own sessions", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  function call(spy: typeof fetch, i = 0): [string, string | undefined] {
    const [url, opts] = (spy as unknown as ReturnType<typeof vi.fn>).mock.calls[i];
    return [String(url), (opts as RequestInit).method];
  }

  it("lists, revokes one and revokes the rest at /account/sessions", async () => {
    const row = {
      token_hash: "h1",
      created_at: "2026-09-01T00:00:00Z",
      expires_at: "2026-10-01T00:00:00Z",
      last_seen_at: "2026-09-24T00:00:00Z",
      user_agent: "UA",
      client_ip: "192.0.2.1",
      current: true,
    };
    let spy = fakeFetch({ sessions: [row] });
    vi.stubGlobal("fetch", spy);
    expect(await api.listMySessions()).toEqual([row]);
    expect(call(spy)).toEqual(["/account/sessions", "GET"]);

    spy = fakeFetch({ ok: true, signed_out: false });
    vi.stubGlobal("fetch", spy);
    expect(await api.revokeMySession("a/b")).toEqual({ ok: true, signed_out: false });
    expect(call(spy)).toEqual(["/account/sessions/a%2Fb", "DELETE"]);

    spy = fakeFetch({ revoked: 2 });
    vi.stubGlobal("fetch", spy);
    expect(await api.revokeMyOtherSessions()).toEqual({ revoked: 2 });
    expect(call(spy)).toEqual(["/account/sessions/revoke-others", "POST"]);
  });

  it("says a session that is already gone has ended", () => {
    expect(humanizeError({ status: 404, code: "session_not_found" })).toBe("That session has already ended.");
  });
});

describe("responses that are not the API's JSON", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  function htmlFetch(status: number, statusText: string) {
    return vi.fn(async () => ({
      ok: status >= 200 && status < 300,
      status,
      statusText,
      text: async () => "<!DOCTYPE html><html><body>Bad gateway</body></html>",
    })) as unknown as typeof fetch;
  }

  it("turns an HTML 502 page into upstream_unavailable with the status kept", async () => {
    vi.stubGlobal("fetch", htmlFetch(502, "Bad Gateway"));
    const err = await api.status("lobby").catch((e) => e);
    expect(err).toMatchObject({ status: 502, code: "upstream_unavailable" });
    expect(humanizeError(err)).toMatch(/unavailable/i);
    expect(humanizeError(err)).not.toMatch(/JSON|DOCTYPE/);
  });

  it("keeps the status of an HTML 401 so the session branch still runs", async () => {
    const target = new EventTarget();
    vi.stubGlobal("window", target);
    let expired = 0;
    target.addEventListener(SESSION_EXPIRED_EVENT, () => (expired += 1));
    vi.stubGlobal("fetch", htmlFetch(401, "Unauthorized"));
    const err = await api.status("lobby").catch((e) => e);
    expect(err).toMatchObject({ status: 401 });
    expect(humanizeError(err)).toMatch(/session/i);
    expect(expired).toBe(1);
  });

  it("refuses a 200 whose body is not JSON", async () => {
    vi.stubGlobal("fetch", htmlFetch(200, "OK"));
    await expect(api.me()).rejects.toMatchObject({ status: 200, code: "upstream_unavailable" });
  });

  it("maps an unknown 5xx code to the unavailable line", () => {
    expect(humanizeError({ status: 500, code: "internal", message: "internal error" })).toMatch(/unavailable/i);
    expect(humanizeError({ status: 413, code: "error", message: "Payload Too Large" })).toMatch(/larger/i);
  });
});

describe("session and connection signals", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  function listen(name: string) {
    const target = new EventTarget();
    vi.stubGlobal("window", target);
    const seen: unknown[] = [];
    target.addEventListener(name, (e) => seen.push((e as CustomEvent).detail ?? true));
    return seen;
  }

  it("announces a 401 from a protected route", async () => {
    const seen = listen(SESSION_EXPIRED_EVENT);
    vi.stubGlobal("fetch", fakeFetch({ error: { code: "unauthorized", message: "x" } }, { ok: false, status: 401 }));
    await expect(api.myServers()).rejects.toMatchObject({ status: 401 });
    expect(seen).toHaveLength(1);
  });

  it("stays silent for /me and the sign-in doors", async () => {
    const seen = listen(SESSION_EXPIRED_EVENT);
    vi.stubGlobal("fetch", fakeFetch({ error: { code: "unauthorized", message: "x" } }, { ok: false, status: 401 }));
    await expect(api.me()).rejects.toMatchObject({ status: 401 });
    await expect(api.setupStatus()).rejects.toMatchObject({ status: 401 });
    expect(seen).toHaveLength(0);
  });

  it("reports a fetch that got no response, and the next one that did", async () => {
    const seen = listen(CONNECTION_EVENT);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    const err = await api.myServers().catch((e) => e);
    expect(err).toMatchObject({ status: 0, code: "network_error" });
    expect(humanizeError(err)).toMatch(/Cloudflare Access/);
    await api.myServers().catch(() => {});
    expect(seen).toEqual([{ ok: false }]);
    expect(isConnectionLost()).toBe(true);

    vi.stubGlobal("fetch", fakeFetch({ servers: [] }));
    await api.myServers();
    expect(seen).toEqual([{ ok: false }, { ok: true }]);
    expect(isConnectionLost()).toBe(false);
  });
});

// The API's generic codes carry an English developer message ("user not found",
// "invalid request"); the panel words them itself so a Chinese UI never shows it.
describe("copy for the generic server codes", () => {
  it("names a missing record, a lost race and a refused format in the UI's words", () => {
    expect(humanizeError({ status: 404, code: "not_found", message: "user not found" })).toMatch(/no longer exists/);
    expect(humanizeError({ status: 409, code: "conflict", message: "conflict" })).toMatch(/changed in the meantime/);
    expect(humanizeError({ status: 409, code: "restore_in_progress", message: "restore running" })).toMatch(
      /still being restored/,
    );
    expect(humanizeError({ status: 415, code: "unsupported_media_type", message: "json only" })).toMatch(
      /format the server does not accept/,
    );
  });

  it("keeps the server's reason for a bad request, and never shows an empty one", () => {
    expect(humanizeError({ status: 400, code: "bad_request", message: "mc_uuid is required" })).toBe(
      "The request was not accepted: mc_uuid is required",
    );
    expect(humanizeError({ status: 400, code: "bad_request", message: "" })).toBe("Something went wrong.");
  });

  it("reads a full upload store as full, not as an outage", () => {
    expect(humanizeError({ status: 507, code: "uploads_full", message: "" })).toMatch(/upload store is full/);
  });

  it("words a failure the panel caught itself from its code", () => {
    const err = clientError("passkey_no_credential");
    expect(err.status).toBe(0);
    expect(humanizeError(err)).toBe("The browser returned no passkey. Try again.");
  });

  it("words the re-authentication refusals", () => {
    expect(humanizeError({ status: 403, code: "reauth_required", message: "raw" })).toBe(
      "Confirm it's you first: this change needs your Passkey or an email code from the last few minutes.",
    );
    expect(humanizeError({ status: 403, code: "staff_reauth", message: "raw" })).toBe(
      "Operator accounts confirm with a Passkey or by signing in again.",
    );
    expect(humanizeError({ status: 400, code: "no_session", message: "raw" })).toBe(
      "This only works in a browser signed in to Felis.",
    );
  });
});
