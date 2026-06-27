import type { IncomingMessage, ServerResponse } from "node:http";
import type { Plugin } from "vite";
import type {
  AutostartPolicy,
  CreateServerRequest,
  Identity,
  LoginResult,
  Phase,
  ServerInfo,
  WhitelistImage,
} from "../src/lib/types";

const ACCOUNT_IDS = ["owner", "user", "linked", "setup"] as const;

type AccountID = (typeof ACCOUNT_IDS)[number];
type Role = "admin" | "user";
type Method = "GET" | "POST";
type CreateError =
  | "bad_request"
  | "already_exists"
  | "subdomain_taken"
  | "image_not_whitelisted";

interface MockAccount {
  id: AccountID;
  role: Role;
  email: string;
  linked: boolean;
  mustChangePassword: boolean;
}

interface MockServer extends ServerInfo {
  owner: AccountID | null;
}

interface MockState {
  accounts: Record<AccountID, MockAccount>;
  servers: MockServer[];
  images: WhitelistImage[];
}

interface RequestContext {
  req: IncomingMessage;
  res: ServerResponse;
  state: MockState;
  method: string;
  parts: string[];
}

interface SessionContext extends RequestContext {
  account: MockAccount;
}

const SESSION_COOKIE = "felis_mock_session";
const ROOT_DOMAIN = "dev.felis.localhost";
const API_BASE = "/api/v1";
const MOCK_PASSWORD = "devpassword";
const MOCK_LINK_CODE = "LINK1234";
const MC_UUID = "00000000-0000-4000-8000-000000000001";
const RESET_ROUTE = `${API_BASE}/__mock/reset`;

const LOGIN_HINT_STYLE = `
#felis-mock-login-hint {
  position: fixed;
  right: 20px;
  bottom: 20px;
  z-index: 2147483647;
  max-width: min(360px, calc(100vw - 40px));
  border: 1px dashed hsl(217 33% 22%);
  border-radius: 8px;
  background: hsl(222 47% 8% / 0.96);
  color: hsl(215 20% 70%);
  box-shadow: 0 18px 60px hsl(222 47% 3% / 0.45);
  padding: 12px;
  font: 12px/1.55 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
#felis-mock-login-hint strong {
  display: block;
  color: hsl(210 40% 98%);
  font-size: 13px;
  margin-bottom: 4px;
}
#felis-mock-login-hint code {
  color: hsl(210 40% 98%);
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}
`;
const LOGIN_HINT_SCRIPT = `
(() => {
  const id = "felis-mock-login-hint";
  const html = '<aside id="' + id + '" aria-label="Mock sign-in credentials"><strong>Mock sign-in</strong><div>Admin: <code>owner</code> / <code>${MOCK_PASSWORD}</code></div><div>User: <code>user</code> / <code>${MOCK_PASSWORD}</code> (not linked)</div><div>User: <code>linked</code> / <code>${MOCK_PASSWORD}</code> (linked)</div><div>First login: <code>setup</code> / <code>${MOCK_PASSWORD}</code></div><div>Link code: <code>${MOCK_LINK_CODE}</code></div></aside>';
  const sync = () => {
    const existing = document.getElementById(id);
    if (location.pathname === "/login") {
      if (!existing) document.body.insertAdjacentHTML("beforeend", html);
      return;
    }
    existing?.remove();
  };
  const notify = () => setTimeout(sync, 0);
  for (const key of ["pushState", "replaceState"]) {
    const original = history[key];
    history[key] = function (...args) {
      const out = original.apply(this, args);
      notify();
      return out;
    };
  }
  addEventListener("popstate", notify);
  addEventListener("DOMContentLoaded", sync);
  sync();
})();
`;

function initialState(): MockState {
  return {
    accounts: {
      owner: account("owner", "admin", true, false),
      user: account("user", "user", false, false),
      linked: account("linked", "user", true, false),
      setup: account("setup", "admin", true, true),
    },
    images: [
      { image_ref: "registry.felis.svc:5000/paper-1.21:demo", enabled: true, source: "demo" },
      { image_ref: "registry.felis.svc:5000/fabric-1.20.1:demo", enabled: true, source: "demo" },
      {
        image_ref: "registry.felis.svc:5000/forge-1.20.1:disabled",
        enabled: false,
        source: "demo",
      },
    ],
    servers: [
      server("survival", "Survival SMP", "Running", "owner", {
        players: 7,
        maxPlayers: 20,
      }),
      server("creative", "Creative Lab", "Stopped", "user", {
        autostartPolicy: "public",
        maxPlayers: 16,
      }),
      server("modded", "Modded Testbed", "Starting", "owner", {
        autostartPolicy: "allowlist",
        maxPlayers: 12,
      }),
      server("claim-me", "Claimable Node", "Stopped", null, {
        maxPlayers: 10,
      }),
    ],
  };
}

function mockStartupMessage(): string {
  return [
    "",
    "  Felis mock API",
    `  API base: ${API_BASE}`,
    `  Root domain: ${ROOT_DOMAIN}`,
    "",
    "  Accounts:",
    `    owner  / ${MOCK_PASSWORD}  admin, linked`,
    `    user   / ${MOCK_PASSWORD}  user, not linked`,
    `    linked / ${MOCK_PASSWORD}  user, linked`,
    `    setup  / ${MOCK_PASSWORD}  admin, first-login password change`,
    "",
    `  Link code: ${MOCK_LINK_CODE}`,
    `  Reset state: curl -X POST http://127.0.0.1:5173${RESET_ROUTE}`,
    "",
  ].join("\n");
}

function account(
  id: AccountID,
  role: Role,
  linked: boolean,
  mustChangePassword: boolean,
): MockAccount {
  return {
    id,
    role,
    linked,
    mustChangePassword,
    email: `${id}@mock.felis.local`,
  };
}

function server(
  name: string,
  displayName: string,
  phase: Phase,
  owner: AccountID | null,
  overrides: Partial<ServerInfo> = {},
): MockServer {
  return {
    name,
    subdomain: name,
    displayName,
    phase,
    desiredState: phase === "Stopped" ? "Stopped" : "Running",
    players: phase === "Running" ? 1 : 0,
    maxPlayers: 20,
    autostartPolicy: "ownerOnly",
    owned: false,
    claimable: false,
    owner,
    ...overrides,
  };
}

function sendJSON(res: ServerResponse, status: number, value: unknown): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(value));
}

function sendError(
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
): void {
  sendJSON(res, status, { error: { code, message } });
}

function route(ctx: RequestContext): string {
  return `${ctx.method} ${ctx.parts.slice(2).join("/")}`;
}

function is(method: Method, ctx: RequestContext): boolean {
  return ctx.method === method;
}

async function readJSON<T>(req: IncomingMessage): Promise<T> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  const text = Buffer.concat(chunks).toString("utf8");
  return (text ? JSON.parse(text) : {}) as T;
}

function readAccount(req: IncomingMessage, state: MockState): MockAccount | null {
  const ids = ACCOUNT_IDS.join("|");
  const m = new RegExp(`(?:^|;\\s*)${SESSION_COOKIE}=(${ids})(?:;|$)`).exec(
    req.headers.cookie ?? "",
  );
  return m ? state.accounts[m[1] as AccountID] : null;
}

function setSessionCookie(res: ServerResponse, accountID: AccountID): void {
  res.setHeader("Set-Cookie", `${SESSION_COOKIE}=${accountID}; Path=/; SameSite=Lax`);
}

function clearSessionCookie(res: ServerResponse): void {
  res.setHeader("Set-Cookie", `${SESSION_COOKIE}=; Path=/; Max-Age=0; SameSite=Lax`);
}

function loginAccount(username: string): AccountID | null {
  const normalized = username.toLowerCase();
  return ACCOUNT_IDS.includes(normalized as AccountID) ? (normalized as AccountID) : null;
}

function identity(accountInfo: MockAccount): Identity {
  return {
    user_id: `mock-${accountInfo.id}`,
    email: accountInfo.email,
    role: accountInfo.role,
    is_admin: accountInfo.role === "admin",
    must_change_password: accountInfo.mustChangePassword,
  };
}

function findServer(state: MockState, name: string): MockServer | null {
  return state.servers.find((s) => s.name === name) ?? null;
}

function canSee(accountInfo: MockAccount, serverInfo: MockServer): boolean {
  return accountInfo.role === "admin" || serverInfo.owner === accountInfo.id || serverInfo.owner === null;
}

function canManage(accountInfo: MockAccount, serverInfo: MockServer): boolean {
  return accountInfo.role === "admin" || serverInfo.owner === accountInfo.id;
}

function visibleServers(state: MockState, accountInfo: MockAccount): ServerInfo[] {
  return state.servers
    .filter((serverInfo) => canSee(accountInfo, serverInfo))
    .map((serverInfo) => projectServer(serverInfo, accountInfo));
}

function projectServer(serverInfo: MockServer, accountInfo: MockAccount): ServerInfo {
  const { owner: _owner, ...wire } = serverInfo;
  const owned = canManage(accountInfo, serverInfo);
  return {
    ...wire,
    owned,
    claimable: serverInfo.owner === null && accountInfo.linked && !owned,
  };
}

function setPhase(serverInfo: MockServer, phase: Phase): void {
  serverInfo.phase = phase;
  serverInfo.desiredState = phase === "Stopped" ? "Stopped" : "Running";
  serverInfo.players = phase === "Running" ? Math.max(serverInfo.players ?? 0, 1) : 0;
}

function policy(value: unknown): AutostartPolicy {
  return value === "public" || value === "allowlist" ? value : "ownerOnly";
}

function createServer(
  state: MockState,
  owner: AccountID,
  req: Partial<CreateServerRequest>,
): MockServer | CreateError {
  const name = req.name?.trim();
  const subdomain = req.subdomain?.trim();
  if (!name || !subdomain || !req.image) return "bad_request";
  if (findServer(state, name)) return "already_exists";
  if (state.servers.some((s) => s.subdomain === subdomain)) return "subdomain_taken";
  if (!state.images.some((i) => i.enabled && i.image_ref === req.image)) {
    return "image_not_whitelisted";
  }

  const created = server(name, req.displayName?.trim() || name, "Stopped", owner, {
    subdomain,
    players: 0,
    maxPlayers: 20,
    autostartPolicy: policy(req.autostartPolicy),
  });
  state.servers.unshift(created);
  return created;
}

function sendCreateError(res: ServerResponse, code: CreateError): void {
  const status = code === "already_exists" || code === "subdomain_taken" ? 409 : 400;
  sendError(res, status, code, code);
}

async function handlePublic(ctx: RequestContext): Promise<boolean> {
  switch (route(ctx)) {
    case "POST auth/login": {
      const body = await readJSON<{ username?: string; password?: string }>(ctx.req);
      const accountID = body.username ? loginAccount(body.username.trim()) : null;
      if (!accountID || body.password !== MOCK_PASSWORD) {
        sendError(ctx.res, 403, "invalid_credentials", "invalid mock credentials");
        return true;
      }
      const accountInfo = ctx.state.accounts[accountID];
      setSessionCookie(ctx.res, accountID);
      const out: LoginResult = {
        user_id: `mock-${accountInfo.id}`,
        role: accountInfo.role,
        must_change_password: accountInfo.mustChangePassword,
      };
      sendJSON(ctx.res, 200, out);
      return true;
    }
    case "POST auth/logout":
      clearSessionCookie(ctx.res);
      sendJSON(ctx.res, 200, { ok: true });
      return true;
    default:
      return false;
  }
}

async function handleSession(ctx: SessionContext): Promise<boolean> {
  switch (route(ctx)) {
    case "GET me/servers":
      sendJSON(ctx.res, 200, { servers: visibleServers(ctx.state, ctx.account) });
      return true;
    case "GET me":
      sendJSON(ctx.res, 200, identity(ctx.account));
      return true;
    case "POST auth/change-password":
      ctx.account.mustChangePassword = false;
      sendJSON(ctx.res, 200, { ok: true });
      return true;
    case "GET images":
      sendJSON(ctx.res, 200, { images: ctx.state.images });
      return true;
    case "POST servers":
      await createServerRoute(ctx);
      return true;
    case "POST account/link/start":
      sendJSON(ctx.res, 200, { linked: ctx.account.linked });
      return true;
    case "POST account/link/verify":
      await verifyLinkRoute(ctx);
      return true;
    default:
      return handleServerRoute(ctx);
  }
}

async function createServerRoute(ctx: SessionContext): Promise<void> {
  if (ctx.account.role !== "admin") {
    sendError(ctx.res, 403, "forbidden", "admin account required");
    return;
  }

  const body = await readJSON<Partial<CreateServerRequest>>(ctx.req);
  const created = createServer(ctx.state, ctx.account.id, body);
  if (typeof created === "string") {
    sendCreateError(ctx.res, created);
    return;
  }
  sendJSON(ctx.res, 201, {
    name: created.name,
    subdomain: created.subdomain,
    desiredState: created.desiredState,
  });
}

async function verifyLinkRoute(ctx: SessionContext): Promise<void> {
  const body = await readJSON<{ code?: string }>(ctx.req);
  if (body.code?.trim().toUpperCase() !== MOCK_LINK_CODE) {
    sendError(ctx.res, 400, "invalid_code", "invalid mock link code");
    return;
  }
  ctx.account.linked = true;
  sendJSON(ctx.res, 200, { linked: true, mc_uuid: MC_UUID });
}

function handleServerRoute(ctx: SessionContext): boolean {
  if (ctx.parts[2] !== "servers" || !ctx.parts[3]) return false;

  const serverInfo = findServer(ctx.state, decodeURIComponent(ctx.parts[3]));
  if (!serverInfo) {
    sendError(ctx.res, 404, "not_found", "server not found");
    return true;
  }
  if (!canSee(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not visible to this account");
    return true;
  }

  if (is("GET", ctx) && ctx.parts[4] === "status") {
    sendJSON(ctx.res, 200, projectServer(serverInfo, ctx.account));
    return true;
  }
  if (is("GET", ctx) && ctx.parts[4] === "console") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    streamConsole(ctx.req, ctx.res, serverInfo);
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "wake") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    setPhase(serverInfo, "Starting");
    sendJSON(ctx.res, 200, { name: serverInfo.name, desiredState: "Running" });
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "stop") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    setPhase(serverInfo, "Stopped");
    sendJSON(ctx.res, 200, { name: serverInfo.name, desiredState: "Stopped" });
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "command") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    handleCommandMock(ctx, serverInfo);
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "claim") {
    claimServer(ctx, serverInfo);
    return true;
  }

  return false;
}

async function handleCommandMock(
  ctx: SessionContext,
  serverInfo: MockServer,
): Promise<void> {
  const body = await readJSON<{ command?: string }>(ctx.req);
  const cmd = body.command?.trim();
  if (!cmd) {
    sendError(ctx.res, 400, "bad_request", "command is required");
    return;
  }
  if (serverInfo.phase !== "Running") {
    sendError(
      ctx.res,
      409,
      "conflict",
      "server is not running; wake it before sending console commands",
    );
    return;
  }
  const reply = mockCommandReply(cmd);
  sendJSON(ctx.res, 200, { output: reply });
}

function mockCommandReply(cmd: string): string {
  const lower = cmd.toLowerCase();
  if (lower === "list") return "There are 2 of a max 20 players online: mock_player, test_player";
  if (lower === "tps" || lower === "forge tps") return "TPS from last 5s, 10s, 1m, 5m, 15m: 20.00, 20.00, *19.87, 19.95, 19.98";
  if (cmd.startsWith("say ")) return `[mock_server] ${cmd.slice(4)}`;
  if (lower === "help") return "--- Showing help ---\n/felis\n/msg\n/list\n/rules";
  return `[mock] command "${cmd}" executed`;
}

function claimServer(ctx: SessionContext, serverInfo: MockServer): void {
  if (serverInfo.owner !== null) {
    sendError(ctx.res, 409, "already_claimed", "server is already claimed");
    return;
  }
  if (!ctx.account.linked) {
    sendError(ctx.res, 412, "not_linked", "link a Minecraft account first");
    return;
  }
  serverInfo.owner = ctx.account.id;
  setPhase(serverInfo, "Starting");
  sendJSON(ctx.res, 200, { name: serverInfo.name, claimed: true });
}

function streamConsole(
  req: IncomingMessage,
  res: ServerResponse,
  serverInfo: MockServer,
): void {
  const lines = [
    `[12:00:00] [Server thread/INFO]: Starting ${serverInfo.displayName ?? serverInfo.name}`,
    "[12:00:01] [Server thread/INFO]: Loading properties",
    "[12:00:02] [Server thread/WARN]: Mock world uses in-memory state only",
    "[12:00:03] [Server thread/INFO]: Preparing spawn area: 100%",
    "[12:00:04] [Server thread/INFO]: Done (4.123s)! For help, type \"help\"",
  ];
  let i = 0;

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
  });
  res.write(": connected\n\n");

  const timer = setInterval(() => {
    res.write(`data: ${lines[i % lines.length]}\n\n`);
    i++;
  }, 900);

  req.on("close", () => clearInterval(timer));
}

export function mockApiPlugin(): Plugin {
  let state = initialState();

  return {
    name: "felis-mock-api",
    transformIndexHtml() {
      return [
        { tag: "style", attrs: { id: "felis-mock-login-style" }, children: LOGIN_HINT_STYLE },
        { tag: "script", children: LOGIN_HINT_SCRIPT },
      ];
    },
    configureServer(server) {
      server.config.logger.info(mockStartupMessage());
      server.middlewares.use(async (req, res, next) => {
        const url = new URL(req.url ?? "/", "http://localhost");
        const method = req.method ?? "GET";
        const parts = url.pathname.split("/").filter(Boolean);

        if (method === "GET" && url.pathname === "/config.json") {
          sendJSON(res, 200, { apiBase: API_BASE, rootDomain: ROOT_DOMAIN });
          return;
        }
        if (parts[0] !== "api" || parts[1] !== "v1") {
          next();
          return;
        }
        if (method === "POST" && parts[2] === "__mock" && parts[3] === "reset") {
          state = initialState();
          server.config.logger.info("  Felis mock API: state reset");
          sendJSON(res, 200, { ok: true });
          return;
        }

        const ctx: RequestContext = { req, res, state, method, parts };
        try {
          if (await handlePublic(ctx)) return;

          const accountInfo = readAccount(req, state);
          if (!accountInfo) {
            sendError(res, 401, "unauthenticated", "mock session required");
            return;
          }
          if (await handleSession({ ...ctx, account: accountInfo })) return;

          sendError(res, 404, "not_found", "mock route not found");
        } catch (err) {
          sendError(res, 500, "mock_error", err instanceof Error ? err.message : "mock error");
        }
      });
    },
  };
}
