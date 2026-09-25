import { createHash } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { gzipSync } from "node:zlib";
import type { Plugin } from "vite";
import type {
  AutostartPolicy,
  BackupView,
  Build,
  BuildScan,
  CreateServerRequest,
  FleetServer,
  Identity,
  MyServerView,
  Phase,
  ServerJob,
  ServerStatus,
  WhitelistImage,
  Submission,
  UserView,
  UserDetail,
  CreateUserRequest,
  PatchUserRequest,
  QuotaView,
  QuotaInput,
  SessionView,
} from "../src/lib/types";

type AccountID = string;
type Role = "admin" | "user" | "owner";
type Method = "GET" | "POST" | "DELETE" | "PATCH" | "PUT";
type CreateError =
  | "bad_request"
  | "already_exists"
  | "subdomain_taken"
  | "image_not_whitelisted";

function isAdmin(role: Role): boolean {
  return role === "admin" || role === "owner";
}

function isOwner(role: Role): boolean {
  return role === "owner";
}

interface MockAccount {
  id: string;
  role: Role;
  email: string;
  linked: boolean;
  emailVerified: boolean;
  disabled?: boolean;
  created_at?: string;
  updated_at?: string;
  quota?: QuotaView;
  sessions?: SessionView[];
  // Until when this browser's session counts as re-authenticated (ms epoch),
  // and the address an email-change code was last sent to.
  reauthUntil?: number;
  pendingEmail?: string;
}

interface MockServer extends ServerStatus {
  owner: AccountID | null;
}

interface PlayerLuckPerms {
  groups: string[];
  permissions: { node: string; value: boolean; world?: string }[];
}

interface AccessState {
  whitelist: string[];
  banned: string[];
  // online is the mock's stand-in for the live RCON "list" roster. Kick and ban
  // splice a player out of it so the demo roster reflects the action on reload.
  online: string[];
  luckperms: Record<string, PlayerLuckPerms>;
}

interface MockState {
  accounts: Record<AccountID, MockAccount>;
  servers: MockServer[];
  images: WhitelistImage[];
  access: Record<string, AccessState>;
  backups: BackupView[];
  // Backup/restore Jobs by server, newest first. The mock finishes each at once.
  jobs: Record<string, ServerJob[]>;
  builds: Build[];
  passkeys: Record<AccountID, { id: string; name: string; created_at: string }[]>;
  submissions: Submission[];
  updateWindow: { start: string | null; end: string | null };
}

// PLAYER_NAME mirrors the backend's mcNameRe (handlers_access.go) so the mock
// rejects a malformed player exactly as the real API would (400 bad_request),
// keeping the panel's error path exercisable in dev.
const PLAYER_NAME = /^[A-Za-z0-9_]{1,16}$/;
const LP_NODE = /^[A-Za-z0-9_.*-]{1,64}$/;
const LP_CTX = /^[A-Za-z0-9_-]{1,48}$/;

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
const MOCK_LINK_CODE = "LINK1234";
const MOCK_OTP_CODE = "123456";
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
  const html = '<aside id="' + id + '" aria-label="Mock sign-in credentials"><strong>Mock sign-in (passwordless)</strong><div>Email OTP: any email / code <code>${MOCK_OTP_CODE}</code> (signs in as <code>owner</code>, admin)</div><div>Link code: <code>${MOCK_LINK_CODE}</code> (signs in as <code>linked</code>, user)</div><div>Passkey: any assertion is accepted (signs in as <code>owner</code>)</div><div>Operator: any email / code <code>${MOCK_OTP_CODE}</code>, approved ~9s later; an address starting <code>expire</code> lapses in 20s</div></aside>';
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

// World-backup seed. A backup is written when the reaper archives an inactive
// world, so these read as "sleep saves": a handful for survival (recent through one
// nearly expired, to exercise the relative-time and near-expiry states), one for
// modded, none for the rest so the empty state shows too. former_owner is the
// archiving owner; GET /backups is scoped by it for non-admins (BackupsForUser).
const GiB = 1024 ** 3;
const DAY_MS = 86_400_000;

// Uploaded build contexts by submission id. Seeded submissions get a small
// stand-in archive so the review page's download and digest check have bytes.
const contextBlobs = new Map<string, Buffer>();

function contextBlob(id: string): Buffer {
  let blob = contextBlobs.get(id);
  if (!blob) {
    blob = gzipSync(Buffer.from(`FROM eclipse-temurin:21-jre\n# mock build context for ${id}\n`));
    contextBlobs.set(id, blob);
  }
  return blob;
}

function sha256Hex(b: Buffer): string {
  return createHash("sha256").update(b).digest("hex");
}
const RETENTION_DAYS = 90;

let freshBackups = 0;

function backup(
  server: string,
  daysAgo: number,
  sizeBytes: number,
  formerOwner: string,
  reason = "inactive_15d",
): BackupView {
  const created = Date.now() - daysAgo * DAY_MS;
  return {
    // A seeded row is named by its age; one made now needs a sequence to stay unique.
    id: daysAgo > 0 ? `bk-${server}-${daysAgo}` : `bk-${server}-now-${++freshBackups}`,
    server_name: server,
    former_owner: formerOwner,
    size_bytes: Math.round(sizeBytes),
    reason,
    status: "present",
    created_at: new Date(created).toISOString(),
    expires_at: new Date(created + RETENTION_DAYS * DAY_MS).toISOString(),
  };
}

function mockBackups(): BackupView[] {
  return [
    backup("survival", 5, 1.4 * GiB, "owner"),
    backup("survival", 20, 1.3 * GiB, "owner"),
    backup("survival", 45, 1.2 * GiB, "owner"),
    backup("survival", 88, 2.1 * GiB, "owner"), // ~2 days from expiry — exercises the urgency state
    backup("modded", 12, 0.6 * GiB, "owner"),
    // The linked player's own server, so a player sees the restore rows too.
    backup("lobby", 2, 0.8 * GiB, "linked", "manual"),
    backup("lobby", 30, 0.7 * GiB, "linked"),
  ];
}

function initialState(): MockState {
  return {
    accounts: {
      owner: account("owner", "owner", true, false),
      user: account("user", "user", false, false),
      linked: account("linked", "user", true, true),
    },
    images: [
      whitelistImage("registry.felis.svc:5000/paper-1.21:demo", "recommended"),
      whitelistImage("registry.felis.svc:5000/fabric-1.20.1:demo", "recommended"),
      { ...whitelistImage("registry.felis.svc:5000/forge-1.20.1:disabled", "external"), enabled: false },
    ],
    servers: [
      server("survival", "Survival SMP", "Running", "owner", {
        playersOnline: 12,
        playersMax: 20,
        autostartPolicy: "public",
        javaMemory: "4Gi",
        storageSize: "20Gi",
        // Pinned the way felis-api stores it: the tag it was created from plus the
        // digest that tag named then.
        image:
          "registry.felis.svc:5000/paper-1.21:demo@sha256:3b637d68843f53750a2e3c2d29c0346b6f81ffd22db2902b80bd5048ee04cc09",
      }),
      server("lobby", "Hub Lobby", "Running", "linked", {
        playersOnline: 28,
        playersMax: 60,
        autostartPolicy: "public",
      }),
      server("creative", "Creative Lab", "Stopped", "user", {
        autostartPolicy: "public",
        playersMax: 16,
      }),
      server("modded", "Modded Testbed", "Starting", "owner", {
        autostartPolicy: "allowlist",
        playersMax: 12,
      }),
      server("broken", "Broken Node", "Failed", "user", {
        autostartPolicy: "ownerOnly",
        playersMax: 8,
      }),
      server("claim-me", "Claimable Node", "Stopped", null, {
        playersMax: 10,
      }),
      ...generatedServers(),
    ],
    access: {
      // Seeded past a page (PAGE_SIZE=10) and the search threshold (>8) so the
      // whitelist's paging + filter are both exercisable in the mock demo.
      survival: {
        whitelist: [
          "mock_player", "test_player", "Notch", "jeb_", "Dinnerbone",
          "Grumm", "Steve", "Alex", "Herobrine", "Technoblade",
          "Dream", "GeorgeNotFound", "Sapnap", "BadBoyHalo", "Skeppy",
          "Tommyinnit", "Tubbo", "Ranboo", "Wilbur_Soot", "Philza",
          "Captain_Puffy", "Nihachu", "Fundy", "Quackity", "Karl_Jacobs",
        ],
        // 12 banned names — past the search threshold (>8) and a page (>10) so the ban
        // list's filter + paging demo too; kept distinct from the online roster so the
        // mock reads like a real server (you don't ban who's currently on).
        banned: [
          "Griefer_99", "tnt_troll", "hack_client_x", "spam_bot_01", "lava_caster",
          "dupe_glitcher", "griefKing", "nukebot", "AFK_farmer", "chat_spammer",
          "xray_cheater", "fly_hacker",
        ],
        // 12 online, matching the server's playersOnline:12 — past the search threshold (>8)
        // and a page (>10) so the roster's filter + paging are both exercisable, with a
        // few non-whitelisted names to try kick / ban on.
        online: [
          "mock_player", "test_player", "Notch", "Steve", "Alex", "jeb_",
          "Dinnerbone", "Griefer_88", "rndGuest_7", "xX_Raider_Xx", "creeper_fan", "Herobrine",
        ],
        luckperms: {
          Steve: {
            groups: ["vip", "default"],
            permissions: [
              { node: "essentials.fly", value: true },
              { node: "essentials.tpa", value: true },
              { node: "minecraft.command.gamemode", value: false },
            ],
          },
          Alex: {
            groups: ["default"],
            permissions: [
              { node: "essentials.tpa", value: true },
            ],
          },
        },
      },
    },
    backups: mockBackups(),
    jobs: {},
    builds: [
      {
        id: "bld-1",
        image_ref: "registry.felis.svc:5000/modpack-beta:1.0",
        status: "succeeded",
        requested_by: "owner@mock.felis.local",
        created_at: new Date(Date.now() - 3600000).toISOString(),
        finished_at: new Date(Date.now() - 3500000).toISOString(),
      },
      {
        id: "bld-2",
        image_ref: "registry.felis.svc:5000/forge-broken:1.0",
        status: "failed",
        error: "the scan blocked the image: 1 CRITICAL, 1 HIGH (CVE-2026-12345, CVE-2025-24813)",
        requested_by: "owner@mock.felis.local",
        created_at: new Date(Date.now() - 1800000).toISOString(),
        finished_at: new Date(Date.now() - 1700000).toISOString(),
      },
      // Started by another admin: the list is the server's, so it shows here too.
      {
        id: "bld-3",
        image_ref: "registry.felis.svc:5000/user-uploads/sub-9:latest",
        status: "building",
        requested_by: "admin2@mock.felis.local",
        created_at: new Date(Date.now() - 300000).toISOString(),
      },
      // Older history, enough to need a second page.
      ...Array.from({ length: 11 }, (_, i): Build => ({
        id: `bld-old-${i + 1}`,
        image_ref: `registry.felis.svc:5000/paper-1.21:r${i + 1}`,
        status: i % 4 === 3 ? "cancelled" : "succeeded",
        requested_by: i % 2 ? "admin2@mock.felis.local" : "owner@mock.felis.local",
        created_at: new Date(Date.now() - (i + 2) * 86400000).toISOString(),
        finished_at: new Date(Date.now() - (i + 2) * 86400000 + 240000).toISOString(),
      })),
    ],
    passkeys: {
      owner: [
        { id: "pk-1", name: "YubiKey 5C", created_at: new Date(Date.now() - 30 * DAY_MS).toISOString() },
      ],
      linked: [],
      user: [],
      setup: [],
    },
    submissions: [
      {
        id: "sub-1",
        submitted_by: "user@mock.felis.local",
        display_name: "Pixelmon Modpack V2",
        context_ref: "minio/contexts/sub-1/context.tar.gz",
        status: "pending_review",
        created_at: new Date(Date.now() - 3600000).toISOString(),
      },
      {
        id: "sub-owner-1",
        submitted_by: "owner@mock.felis.local",
        display_name: "RLCraft Survival Pack",
        context_ref: "minio/contexts/sub-owner-1/context.tar.gz",
        context_sha256: sha256Hex(contextBlob("sub-owner-1")),
        status: "pending_review",
        created_at: new Date(Date.now() - 1800000).toISOString(),
      },
      {
        id: "sub-owner-2",
        submitted_by: "owner@mock.felis.local",
        display_name: "ATM 9 Server Pack",
        context_ref: "minio/contexts/sub-owner-2/context.tar.gz",
        context_sha256: sha256Hex(contextBlob("sub-owner-2")),
        status: "approved",
        image_ref: "registry.felis.svc:5000/user-uploads/sub-owner-2:latest",
        build_id: "bld-3",
        reviewed_by: "setup@mock.felis.local",
        created_at: new Date(Date.now() - 86400000).toISOString(),
        reviewed_at: new Date(Date.now() - 86300000).toISOString(),
      },
      {
        id: "sub-owner-3",
        submitted_by: "owner@mock.felis.local",
        display_name: "Oversized Custom Modpack",
        context_ref: "minio/contexts/sub-owner-3/context.tar.gz",
        context_sha256: sha256Hex(contextBlob("sub-owner-3")),
        status: "rejected",
        reviewed_by: "setup@mock.felis.local",
        created_at: new Date(Date.now() - 172800000).toISOString(),
        reviewed_at: new Date(Date.now() - 172700000).toISOString(),
        reject_reason: "Compressed file size exceeds the platform-wide 1GB threshold. Please reduce the size by removing heavy client-only mods (e.g. resourcepacks or local shaders).",
      },
      {
        id: "sub-2",
        submitted_by: "linked@mock.felis.local",
        display_name: "Create: Astral pack",
        context_ref: "minio/contexts/sub-2/context.tar.gz",
        context_sha256: sha256Hex(contextBlob("sub-2")),
        status: "approved",
        image_ref: "registry.felis.svc:5000/user-uploads/sub-2:latest",
        build_id: "bld-1",
        reviewed_by: "owner@mock.felis.local",
        created_at: new Date(Date.now() - 7200000).toISOString(),
        reviewed_at: new Date(Date.now() - 7100000).toISOString(),
      },
      {
        id: "sub-3",
        submitted_by: "user@mock.felis.local",
        display_name: "Dangerous Modpack (Exploitative)",
        context_ref: "minio/contexts/sub-3/context.tar.gz",
        context_sha256: sha256Hex(contextBlob("sub-3")),
        status: "rejected",
        reviewed_by: "owner@mock.felis.local",
        reject_reason: "Contains malicious code in scripts/run.sh that tries to download remote malware.",
        created_at: new Date(Date.now() - 10800000).toISOString(),
        reviewed_at: new Date(Date.now() - 10700000).toISOString(),
      },
    ],
    updateWindow: { start: null, end: null },
  };
}

// generatedServers fills the mock fleet past one page so the SysAdmin cockpit's
// pagination and fuzzy search are actually exercisable in dev. Deterministic (no
// Math.random) so the demo is stable across reloads: phase / owner / policy /
// capacity all cycle. 8 themes × 3 = 24 servers; with the 6 hand-authored ones the
// fleet is 30 → two pages at PAGE_SIZE 20.
function generatedServers(): MockServer[] {
  const phases: Phase[] = ["Running", "Stopped", "Starting", "Failed", "Running", "Stopped"];
  const owners: (AccountID | null)[] = ["owner", "linked", "user", null];
  const policies: AutostartPolicy[] = ["ownerOnly", "public", "allowlist"];
  const themes = ["smp", "creative", "skyblock", "anarchy", "minigames", "build", "pvp", "vanilla"];
  const out: MockServer[] = [];
  let i = 0;
  for (const theme of themes) {
    for (let n = 1; n <= 3; n++) {
      const phase = phases[i % phases.length];
      const max = 10 + ((i * 7) % 50);
      out.push(
        server(`${theme}-${String(n).padStart(2, "0")}`, `${theme} #${n}`, phase, owners[i % owners.length], {
          playersOnline: phase === "Running" ? 1 + ((i * 3) % max) : 0,
          playersMax: max,
          autostartPolicy: policies[i % policies.length],
        }),
      );
      i++;
    }
  }
  return out;
}

function mockStartupMessage(): string {
  return [
    "",
    "  Felis mock API",
    `  API base: ${API_BASE}`,
    `  Root domain: ${ROOT_DOMAIN}`,
    "",
    "  Sign-in (passwordless):",
    `    Email OTP: any email / code ${MOCK_OTP_CODE}  → owner (admin, linked)`,
    `    Link code: ${MOCK_LINK_CODE}                  → linked (user, linked)`,
    "    Passkey:   any assertion accepted     → owner (admin, linked)",
    `    Op login:  any email / code ${MOCK_OTP_CODE}, approved ~9s later; an email starting "expire" lapses in 20s`,
    `  Reset state: curl -X POST http://127.0.0.1:5173${RESET_ROUTE}`,
    "",
  ].join("\n");
}

function account(
  id: AccountID,
  role: Role,
  linked: boolean,
  emailVerified: boolean,
): MockAccount {
  return {
    id,
    role,
    linked,
    emailVerified,
    email: `${id}@mock.felis.local`,
  };
}

function whitelistImage(image_ref: string, source: string): WhitelistImage {
  return { image_ref, enabled: true, source, added_by: "owner", added_at: "2026-01-01T00:00:00Z" };
}

function server(
  name: string,
  displayName: string,
  phase: Phase,
  owner: AccountID | null,
  overrides: Partial<ServerStatus> = {},
): MockServer {
  return {
    name,
    subdomain: name,
    displayName,
    phase,
    ready: phase === "Running",
    desiredState: phase === "Stopped" ? "Stopped" : "Running",
    playersOnline: phase === "Running" ? 1 : 0,
    playersMax: 20,
    autostartPolicy: "ownerOnly",
    idleStopSeconds: 900,
    owner,
    ...overrides,
  };
}

// mockScan is the scan build.Builder.Scan would keep for the seeded builds.
function mockScan(b: Build): BuildScan | null {
  const at = b.finished_at ?? b.created_at;
  if (b.id === "bld-2") {
    return {
      build_id: b.id, scanned_at: at, has_report: true, has_sbom: false,
      summary: {
        policy: { fail_on: ["CRITICAL", "HIGH"], fail_unfixed: false, accept: ["CVE-2021-35515", "CVE-2025-67030"] },
        blocked: true, packages: 214,
        counts: { CRITICAL: 1, HIGH: 4, MEDIUM: 6, LOW: 2 },
        blocking_counts: { CRITICAL: 1, HIGH: 1 },
        omitted: ["sbom"],
        findings: [
          { id: "CVE-2026-12345", kind: "vulnerability", severity: "CRITICAL", package: "net.minecraftforge:forge",
            installed: "47.1.0", fixed: "47.1.3", target: "libraries/net/minecraftforge/forge/47.1.0/forge.jar", blocking: true },
          { id: "CVE-2025-24813", kind: "vulnerability", severity: "HIGH", package: "org.apache.tomcat.embed:tomcat-embed-core",
            installed: "9.0.97", fixed: "9.0.99, 10.1.35", target: "mods/webmap-2.4.jar", blocking: true },
          { id: "CVE-2021-35515", kind: "vulnerability", severity: "HIGH", package: "org.apache.commons:commons-compress",
            installed: "1.5", fixed: "1.21", target: "paper/paper.jar", blocking: false, accepted: true },
          { id: "CVE-2024-6763", kind: "vulnerability", severity: "HIGH", package: "org.eclipse.jetty:jetty-http",
            installed: "9.4.53.v20231009", target: "mods/webmap-2.4.jar", blocking: false },
          { id: "CVE-2023-2976", kind: "vulnerability", severity: "HIGH", package: "com.google.guava:guava",
            installed: "31.1-jre", target: "libraries/com/google/guava/guava/31.1-jre/guava-31.1-jre.jar", blocking: false },
          { id: "CVE-2024-47554", kind: "vulnerability", severity: "MEDIUM", package: "commons-io:commons-io",
            installed: "2.11.0", fixed: "2.14.0", target: "libraries/commons-io/commons-io/2.11.0/commons-io-2.11.0.jar", blocking: false },
          { id: "CVE-2020-8908", kind: "vulnerability", severity: "LOW", package: "com.google.guava:guava",
            installed: "31.1-jre", fixed: "32.0.0-android", target: "libraries/com/google/guava/guava/31.1-jre/guava-31.1-jre.jar", blocking: false },
        ],
      },
    };
  }
  if (b.id === "bld-1") {
    return {
      build_id: b.id, scanned_at: at, has_report: true, has_sbom: true,
      summary: {
        policy: { fail_on: ["CRITICAL"], fail_unfixed: false },
        blocked: false, packages: 187,
        counts: { MEDIUM: 1, LOW: 1 },
        blocking_counts: {},
        findings: [
          { id: "CVE-2024-47554", kind: "vulnerability", severity: "MEDIUM", package: "commons-io:commons-io",
            installed: "2.11.0", fixed: "2.14.0", target: "libraries/commons-io/commons-io/2.11.0/commons-io-2.11.0.jar", blocking: false },
          { id: "CVE-2020-8908", kind: "vulnerability", severity: "LOW", package: "com.google.guava:guava",
            installed: "31.1-jre", fixed: "32.0.0-android", target: "libraries/com/google/guava/guava/31.1-jre/guava-31.1-jre.jar", blocking: false },
        ],
      },
    };
  }
  return null;
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
  const m = new RegExp(`(?:^|;\\s*)${SESSION_COOKIE}=([a-zA-Z0-9_-]+)(?:;|$)`).exec(
    req.headers.cookie ?? "",
  );
  return m ? state.accounts[m[1]] : null;
}

function setSessionCookie(res: ServerResponse, accountID: string): void {
  res.setHeader("Set-Cookie", `${SESSION_COOKIE}=${accountID}; Path=/; SameSite=Lax`);
}

// signInProven opens a session through a door that proved a factor (email code,
// passkey, operator login), which like the real API counts as a re-auth.
function signInProven(ctx: RequestContext, accountID: AccountID): void {
  setSessionCookie(ctx.res, accountID);
  const acc = ctx.state.accounts[accountID];
  if (acc) acc.reauthUntil = Date.now() + REAUTH_MS;
}

const REAUTH_MS = 5 * 60_000;

/** reauthFactors mirrors reauthState: a passkey, an email code to a verified
 *  address (users) or a fresh sign-in (operators). None → nothing to re-prove. */
function reauthFactors(state: MockState, acc: MockAccount): string[] {
  const hasPasskey = (state.passkeys[acc.id] ?? []).length > 0;
  if (!hasPasskey && !acc.emailVerified) return [];
  const factors = hasPasskey ? ["passkey"] : [];
  if (acc.role !== "user") factors.push("sign_in");
  else if (acc.emailVerified) factors.push("email");
  return factors;
}

/** refusedForReauth answers 403 reauth_required, as the real API does, for a
 *  change to how the account signs in without a recent proof. */
function refusedForReauth(ctx: SessionContext): boolean {
  const acc = ctx.account;
  if (reauthFactors(ctx.state, acc).length === 0 || (acc.reauthUntil ?? 0) > Date.now()) return false;
  sendError(ctx.res, 403, "reauth_required", "confirm it's you first: this change needs your passkey or email code from the last few minutes");
  return true;
}

function markReauth(ctx: SessionContext): void {
  ctx.account.reauthUntil = Date.now() + REAUTH_MS;
  sendJSON(ctx.res, 200, { ok: true, until: new Date(ctx.account.reauthUntil).toISOString() });
}

function clearSessionCookie(res: ServerResponse): void {
  res.setHeader("Set-Cookie", `${SESSION_COOKIE}=; Path=/; Max-Age=0; SameSite=Lax`);
}

// thisSessionHash is the session the mock cookie stands for: one per account,
// so the account page can mark "This device".
function thisSessionHash(acc: MockAccount): string {
  return `mock-this-${acc.id}`;
}

/** accountSessions seeds, on first read, the browsers an account is signed in
 *  on: this one, a phone seen yesterday and a PC idle for a week. */
function accountSessions(acc: MockAccount): SessionView[] {
  if (!acc.sessions) {
    const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
    const until = new Date(Date.now() + 30 * 86_400_000).toISOString();
    acc.sessions = [
      {
        token_hash: thisSessionHash(acc),
        created_at: ago(3 * 86_400_000),
        expires_at: until,
        last_seen_at: ago(0),
        user_agent:
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
        client_ip: "2001:db8::1",
      },
      {
        token_hash: `mock-phone-${acc.id}`,
        created_at: ago(9 * 86_400_000),
        expires_at: until,
        last_seen_at: ago(20 * 3_600_000),
        user_agent:
          "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
        client_ip: "203.0.113.24",
      },
      {
        token_hash: `mock-pc-${acc.id}`,
        created_at: ago(20 * 86_400_000),
        expires_at: until,
        last_seen_at: ago(7 * 86_400_000),
        user_agent:
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.2739.42",
        client_ip: "198.51.100.7",
      },
    ];
  }
  return acc.sessions;
}

function identity(accountInfo: MockAccount): Identity {
  return {
    user_id: `mock-${accountInfo.id}`,
    email: accountInfo.email,
    role: accountInfo.role,
    is_admin: isAdmin(accountInfo.role),
    is_owner: isOwner(accountInfo.role),
    email_verified: accountInfo.emailVerified,
  };
}

function findServer(state: MockState, name: string): MockServer | null {
  return state.servers.find((s) => s.name === name) ?? null;
}

function canSee(accountInfo: MockAccount, serverInfo: MockServer): boolean {
  return isAdmin(accountInfo.role) || serverInfo.owner === accountInfo.id || serverInfo.owner === null;
}

function canManage(accountInfo: MockAccount, serverInfo: MockServer): boolean {
  return isAdmin(accountInfo.role) || serverInfo.owner === accountInfo.id;
}

function visibleServers(state: MockState, accountInfo: MockAccount): MyServerView[] {
  return state.servers
    .filter((serverInfo) => canSee(accountInfo, serverInfo))
    .map((serverInfo) => myServerView(serverInfo, accountInfo));
}

// fleetView projects the internal mock servers into the GET /fleet wire shape
// (the SysAdmin cockpit's read). It is the mock mirror of the Go fleetServerView:
// the CRD field names (playersOnline/playersMax, ready, endpoint*) plus the
// runtime `ready`/`endpoint*` fields, and the owner joined as the email
// (COALESCE(email, username) server-side). Endpoint and live player counts are
// gated on Running, exactly as the real cluster reports them. Ownership is
// decided by account id for the caller, as the Go handler does.
function fleetView(state: MockState, accountInfo: MockAccount): FleetServer[] {
  return state.servers.map((s, i) => {
    const { owner, ...wire } = s;
    return {
      ...wire,
      endpointMode: "domain",
      endpointAddress: s.ready ? `10.43.0.${10 + i}:25565` : undefined,
      playersOnline: s.ready ? s.playersOnline : 0,
      owner: owner ? state.accounts[owner].email : "",
      owned: owner === accountInfo.id,
      claimable: owner === null,
    };
  });
}

// myServerView mirrors handleMyServers: ownership and claim state plus the live
// fields, with owner detail (desired state, policy, unreadable count) only on
// rows the caller owns.
function myServerView(serverInfo: MockServer, accountInfo: MockAccount): MyServerView {
  const owned = canManage(accountInfo, serverInfo);
  return {
    name: serverInfo.name,
    subdomain: serverInfo.subdomain,
    owned,
    claimable: serverInfo.owner === null && accountInfo.linked && !owned,
    phase: serverInfo.phase,
    playersOnline: serverInfo.playersOnline,
    playersMax: serverInfo.playersMax,
    displayName: serverInfo.displayName,
    ...(owned && {
      desiredState: serverInfo.desiredState,
      autostartPolicy: serverInfo.autostartPolicy,
      playerCountUnknown: serverInfo.playerCountUnknown,
    }),
  };
}

// statusView mirrors handleServerStatus: the whole projection for the owner or
// an admin, the public subset (publicServerInfo) for anyone else.
function statusView(serverInfo: MockServer, accountInfo: MockAccount): ServerStatus {
  const { owner: _owner, ...wire } = serverInfo;
  if (canManage(accountInfo, serverInfo)) return wire;
  return {
    name: wire.name,
    subdomain: wire.subdomain,
    displayName: wire.displayName,
    phase: wire.phase,
    ready: wire.ready,
    playersOnline: wire.playersOnline,
    playersMax: wire.playersMax,
    idleStopSeconds: 0,
  };
}

function setPhase(serverInfo: MockServer, phase: Phase): void {
  serverInfo.phase = phase;
  serverInfo.ready = phase === "Running";
  serverInfo.desiredState = phase === "Stopped" ? "Stopped" : "Running";
  serverInfo.playersOnline = phase === "Running" ? Math.max(serverInfo.playersOnline ?? 0, 1) : 0;
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
    playersOnline: 0,
    playersMax: 20,
    autostartPolicy: policy(req.autostartPolicy),
  });
  state.servers.unshift(created);
  return created;
}

function sendCreateError(res: ServerResponse, code: CreateError): void {
  const status = code === "already_exists" || code === "subdomain_taken" ? 409 : 400;
  sendError(res, status, code, code);
}

// Op-login requests the mock has handed out. Nobody is in-game to vouch, so each
// one approves itself a few seconds after start; an "expire…" address never does
// and lapses quickly, to show the page's expiry path.
const opLogins = new Map<string, { approveAt: number; expiresAt: number }>();

async function handlePublic(ctx: RequestContext): Promise<boolean> {
  const opStatus = route(ctx).match(/^GET auth\/op-login\/status\/(.+)$/);
  if (opStatus) {
    const req = opLogins.get(opStatus[1]);
    const now = Date.now();
    sendJSON(ctx.res, 200, { approved: !!req && now >= req.approveAt && now < req.expiresAt });
    return true;
  }
  switch (route(ctx)) {
    case "POST auth/op-login/start": {
      const body = await readJSON<{ email?: string }>(ctx.req);
      if (!body.email || !body.email.includes("@")) {
        sendError(ctx.res, 400, "bad_request", "email is required");
        return true;
      }
      const lapses = body.email.startsWith("expire");
      const id = `op-${opLogins.size + 1}`;
      const now = Date.now();
      const expiresAt = now + (lapses ? 20_000 : 600_000);
      opLogins.set(id, { approveAt: lapses ? Infinity : now + 9_000, expiresAt });
      sendJSON(ctx.res, 202, { request_id: id, expires_at: new Date(expiresAt).toISOString() });
      return true;
    }
    case "POST auth/op-login/finish": {
      const body = await readJSON<{ request_id?: string; code?: string }>(ctx.req);
      const req = body.request_id ? opLogins.get(body.request_id) : undefined;
      const now = Date.now();
      if (!req || now < req.approveAt || now >= req.expiresAt || body.code !== MOCK_OTP_CODE) {
        sendError(ctx.res, 400, "op_login_invalid", "operator login could not be completed");
        return true;
      }
      opLogins.delete(body.request_id!);
      signInProven(ctx, "owner");
      sendJSON(ctx.res, 200, { user_id: "mock-owner", role: "owner" });
      return true;
    }
    case "POST auth/bind": {
      const body = await readJSON<{ code?: string }>(ctx.req);
      const code = body.code?.trim().toUpperCase();
      if (!code) {
        sendError(ctx.res, 400, "bad_request", "code is required");
        return true;
      }
      if (code !== MOCK_LINK_CODE) {
        sendError(ctx.res, 400, "invalid_code", "bind code is invalid or expired");
        return true;
      }
      setSessionCookie(ctx.res, "linked");
      sendJSON(ctx.res, 200, {
        user_id: "mock-linked",
        linked: true,
        mc_uuid: MC_UUID,
        auth_source: "mojang",
      });
      return true;
    }
    case "POST auth/passkey/login/discoverable/begin": {
      sendJSON(ctx.res, 200, {
        publicKey: {
          challenge: "c29tZV9kaXNjb3ZlcmFibGVfY2hhbGxlbmdl",
          rp: { name: "Felis Dev", id: "dev.felis.localhost" },
          user: { id: "bW9ja191c2VyX2lk", name: "owner@mock.felis.local", displayName: "owner" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          allowCredentials: [],
          timeout: 60000,
        },
        login_id: "mock_login_id_1234"
      });
      return true;
    }
    case "POST auth/passkey/login/discoverable/finish": {
      const body = await readJSON<{ login_id?: string; assertion?: any }>(ctx.req);
      if (!body.login_id || !body.assertion) {
        sendError(ctx.res, 400, "bad_request", "login_id and assertion are required");
        return true;
      }
      signInProven(ctx, "owner");
      sendJSON(ctx.res, 200, {
        user_id: "mock-owner",
        role: "owner"
      });
      return true;
    }
    case "POST auth/passkey/login/begin": {
      const body = await readJSON<{ email?: string }>(ctx.req);
      if (!body.email || !body.email.includes("@")) {
        sendError(ctx.res, 400, "bad_request", "email is required");
        return true;
      }
      sendJSON(ctx.res, 200, {
        challenge: "c29tZV9wYXNza2V5X2NoYWxsZW5nZQ",
        rp: { name: "Felis Dev", id: "dev.felis.localhost" },
        user: { id: "bW9ja191c2VyX2lk", name: body.email, displayName: "owner" },
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        allowCredentials: [{ type: "public-key", id: "cGstMQ" }],
        timeout: 60000,
      });
      return true;
    }
    case "POST auth/passkey/login/finish": {
      const body = await readJSON<{ email?: string; assertion?: any }>(ctx.req);
      if (!body.email || !body.assertion) {
        sendError(ctx.res, 400, "bad_request", "email and assertion are required");
        return true;
      }
      signInProven(ctx, "owner");
      sendJSON(ctx.res, 200, {
        user_id: "mock-owner",
        role: "owner"
      });
      return true;
    }
    case "POST auth/email/start": {
      const body = await readJSON<{ email?: string }>(ctx.req);
      if (!body.email || !body.email.includes("@")) {
        sendError(ctx.res, 400, "bad_request", "email is required");
        return true;
      }
      sendJSON(ctx.res, 202, { sent: true, expires_at: new Date(Date.now() + 600000).toISOString() });
      return true;
    }
    case "POST auth/email/verify": {
      const body = await readJSON<{ email?: string; code?: string }>(ctx.req);
      if (!body.email || body.code !== MOCK_OTP_CODE) {
        sendError(ctx.res, 400, "invalid_code", "email code is invalid or expired");
        return true;
      }
      signInProven(ctx, "owner");
      sendJSON(ctx.res, 200, {
        user_id: "mock-owner",
        role: "owner"
      });
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
    case "GET platform/db-backup": {
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      // Yesterday's daily run: fresh, so the card shows its healthy state.
      const at = new Date(Date.now() - 9 * 3600 * 1000);
      const stamp = at.toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
      sendJSON(ctx.res, 200, {
        last: {
          at: at.toISOString(),
          name: `felis-db-${stamp}-daily.tar`,
          label: "daily",
          size_bytes: 3_482_112,
          felis_version: "dev",
          schema_version: 31,
          dir: "/var/lib/felis/db-backups",
        },
        stale: false,
        max_age_seconds: 26 * 3600,
      });
      return true;
    }
    case "GET updates/window":
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      sendJSON(ctx.res, 200, ctx.state.updateWindow);
      return true;
    case "PUT updates/window": {
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      const body = await readJSON<{ start: string | null; end: string | null }>(ctx.req);
      if (body.start === null && body.end === null) {
        ctx.state.updateWindow = { start: null, end: null };
      } else if (body.start === null || body.end === null) {
        sendError(ctx.res, 400, "bad_request", "start and end must both be set or both be null");
        return true;
      } else {
        const start = new Date(body.start);
        const end = new Date(body.end);
        if (isNaN(start.getTime()) || isNaN(end.getTime())) {
          sendError(ctx.res, 400, "bad_request", "invalid start or end time format");
          return true;
        }
        if (end <= start) {
          sendError(ctx.res, 400, "bad_request", "end must be after start");
          return true;
        }
        ctx.state.updateWindow = { start: body.start, end: body.end };
      }
      sendJSON(ctx.res, 200, ctx.state.updateWindow);
      return true;
    }
    case "GET me/servers":
      sendJSON(ctx.res, 200, { servers: visibleServers(ctx.state, ctx.account) });
      return true;
    case "GET me":
      sendJSON(ctx.res, 200, identity(ctx.account));
      return true;
    case "GET fleet":
      // Admin-tier, fleet-wide — mirrors the real adminOnly gate (a non-admin is
      // 403'd before the handler) so the cockpit's RequireAdmin path is exercised.
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      sendJSON(ctx.res, 200, { servers: fleetView(ctx.state, ctx.account) });
      return true;
    case "GET backups":
      {
        // Admin sees every archive; a user only worlds they formerly owned — mirrors
        // AllBackups vs BackupsForUser. server narrows inside that scope, and rows
        // come a page at a time, newest first, with the matching total.
        const url = new URL(ctx.req.url ?? "/", "http://localhost");
        const server = url.searchParams.get("server") ?? "";
        const limit = Math.min(Number(url.searchParams.get("limit")) || 20, 100);
        const offset = Math.max(Number(url.searchParams.get("offset")) || 0, 0);
        const matched = ctx.state.backups
          .filter((b) => b.status === "present")
          .filter((b) => isAdmin(ctx.account.role) || b.former_owner === ctx.account.id)
          .filter((b) => !server || b.server_name === server)
          .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
        sendJSON(ctx.res, 200, { backups: matched.slice(offset, offset + limit), total: matched.length });
      }
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
    case "POST account/email/start": {
      const body = await readJSON<{ email?: string }>(ctx.req);
      if (!body.email || !body.email.includes("@")) {
        sendError(ctx.res, 400, "bad_request", "invalid email");
        return true;
      }
      if (refusedForReauth(ctx)) return true;
      ctx.account.pendingEmail = body.email.trim();
      sendJSON(ctx.res, 202, { sent: true, expires_at: new Date(Date.now() + 600000).toISOString() });
      return true;
    }
    case "POST account/email/verify": {
      const body = await readJSON<{ code?: string }>(ctx.req);
      if (body.code?.trim() !== MOCK_OTP_CODE) {
        sendError(ctx.res, 400, "invalid_code", "email code is invalid or expired");
        return true;
      }
      const next = ctx.account.pendingEmail ?? ctx.account.email;
      // Like the real API: replacing a verified address signs the other devices
      // out, and proving a code counts as a re-auth.
      if (ctx.account.emailVerified && next.toLowerCase() !== ctx.account.email.toLowerCase()) {
        const here = thisSessionHash(ctx.account);
        ctx.account.sessions = accountSessions(ctx.account).filter((s) => s.token_hash === here);
      }
      ctx.account.email = next;
      ctx.account.pendingEmail = undefined;
      ctx.account.emailVerified = true;
      ctx.account.reauthUntil = Date.now() + REAUTH_MS;
      sendJSON(ctx.res, 200, { verified: true, email: ctx.account.email });
      return true;
    }
    case "POST account/passkey/register/begin": {
      if (refusedForReauth(ctx)) return true;
      sendJSON(ctx.res, 200, {
        challenge: "c29tZV9jaGFsbGVuZ2VfZGF0YQ",
        rp: { name: "Felis Dev" },
        user: {
          id: "bW9ja191c2VyX2lk",
          name: ctx.account.email,
          displayName: ctx.account.email,
        },
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      });
      return true;
    }
    case "POST account/passkey/register/finish": {
      const body = await readJSON<{ name?: string; attestation?: any }>(ctx.req);
      if (!body.name || !body.attestation) {
        sendError(ctx.res, 400, "bad_request", "name and attestation are required");
        return true;
      }
      const newCred = {
        id: `pk-${Date.now()}`,
        name: body.name.trim(),
        created_at: new Date().toISOString(),
      };
      if (!ctx.state.passkeys[ctx.account.id]) {
        ctx.state.passkeys[ctx.account.id] = [];
      }
      ctx.state.passkeys[ctx.account.id].unshift(newCred);
      ctx.account.reauthUntil = Date.now() + REAUTH_MS;
      sendJSON(ctx.res, 201, newCred);
      return true;
    }
    case "GET account/passkey/credentials": {
      const list = ctx.state.passkeys[ctx.account.id] ?? [];
      sendJSON(ctx.res, 200, { credentials: list });
      return true;
    }
    case "GET account/reauth": {
      const factors = reauthFactors(ctx.state, ctx.account);
      const until = ctx.account.reauthUntil ?? 0;
      if (factors.length === 0) sendJSON(ctx.res, 200, { needed: false, factors });
      else if (until > Date.now()) sendJSON(ctx.res, 200, { needed: false, until: new Date(until).toISOString(), factors });
      else sendJSON(ctx.res, 200, { needed: true, factors });
      return true;
    }
    case "POST account/reauth/passkey/begin": {
      const list = ctx.state.passkeys[ctx.account.id] ?? [];
      if (list.length === 0) {
        sendError(ctx.res, 409, "no_passkey", "no passkey enrolled; confirm with an email code instead");
        return true;
      }
      sendJSON(ctx.res, 200, {
        publicKey: {
          challenge: "c29tZV9yZWF1dGhfY2hhbGxlbmdl",
          rpId: "dev.felis.localhost",
          allowCredentials: list.map(() => ({ type: "public-key", id: "cGstMQ" })),
          userVerification: "preferred",
          timeout: 60000,
        },
      });
      return true;
    }
    case "POST account/reauth/passkey/finish": {
      const body = await readJSON<{ assertion?: unknown }>(ctx.req);
      if (!body.assertion) {
        sendError(ctx.res, 400, "bad_request", "assertion is required");
        return true;
      }
      markReauth(ctx);
      return true;
    }
    case "POST account/reauth/email/start": {
      if (ctx.account.role !== "user") {
        sendError(ctx.res, 403, "staff_reauth", "operators confirm with a passkey or by signing in again");
        return true;
      }
      if (!ctx.account.emailVerified) {
        sendError(ctx.res, 409, "no_step_up_factor", "no verified email to send a code to");
        return true;
      }
      sendJSON(ctx.res, 202, { sent: true, expires_at: new Date(Date.now() + 600000).toISOString() });
      return true;
    }
    case "POST account/reauth/email/verify": {
      const body = await readJSON<{ code?: string }>(ctx.req);
      if (body.code?.trim() !== MOCK_OTP_CODE) {
        sendError(ctx.res, 400, "invalid_code", "email code is invalid or expired");
        return true;
      }
      markReauth(ctx);
      return true;
    }
    case "GET account/migrate":
      // No migration pending: the real API's answer until one is started in-game.
      sendJSON(ctx.res, 200, { active: false });
      return true;
    case "GET account/sessions": {
      const here = thisSessionHash(ctx.account);
      const sessions = accountSessions(ctx.account)
        .map((s) => (s.token_hash === here ? { ...s, last_seen_at: new Date().toISOString(), current: true } : s))
        .sort((a, b) => b.last_seen_at.localeCompare(a.last_seen_at));
      sendJSON(ctx.res, 200, { sessions });
      return true;
    }
    case "POST account/sessions/revoke-others": {
      const here = thisSessionHash(ctx.account);
      const before = accountSessions(ctx.account);
      ctx.account.sessions = before.filter((s) => s.token_hash === here);
      sendJSON(ctx.res, 200, { revoked: before.length - ctx.account.sessions.length });
      return true;
    }
    default:
      if (ctx.method === "DELETE" && ctx.parts[2] === "account" && ctx.parts[3] === "sessions" && ctx.parts[4]) {
        const hash = ctx.parts[4];
        const list = accountSessions(ctx.account);
        if (!list.some((s) => s.token_hash === hash)) {
          sendError(ctx.res, 404, "session_not_found", "that session has already ended or is not one of yours");
          return true;
        }
        ctx.account.sessions = list.filter((s) => s.token_hash !== hash);
        const signedOut = hash === thisSessionHash(ctx.account);
        if (signedOut) clearSessionCookie(ctx.res);
        sendJSON(ctx.res, 200, { ok: true, signed_out: signedOut });
        return true;
      }
      if (ctx.method === "DELETE" && ctx.parts[2] === "account" && ctx.parts[3] === "passkey" && ctx.parts[4] === "credentials" && ctx.parts[5]) {
        // The real API asks for the re-auth before it looks the passkey up.
        if (refusedForReauth(ctx)) return true;
        const id = ctx.parts[5];
        const list = ctx.state.passkeys[ctx.account.id] ?? [];
        const idx = list.findIndex((k) => k.id === id);
        if (idx < 0) {
          sendError(ctx.res, 404, "not_found", "passkey not found");
          return true;
        }
        // Same rule as the real API: the only passkey of an account without a
        // verified email stays, or the account would have no way to sign in.
        if (list.length === 1 && !ctx.account.emailVerified) {
          sendError(ctx.res, 409, "last_passkey", "this is your only passkey and your email is not verified");
          return true;
        }
        list.splice(idx, 1);
        // Like the real API, removing a passkey signs the other devices out.
        const here = thisSessionHash(ctx.account);
        ctx.account.sessions = accountSessions(ctx.account).filter((s) => s.token_hash === here);
        ctx.res.statusCode = 204;
        ctx.res.end();
        return true;
      }
      if (await handleUserRoute(ctx)) return true;
      if (await handleImageRoute(ctx)) return true;
      if (await handleSubmissionRoute(ctx)) return true;
      return await handleServerRoute(ctx);
  }
}

async function handleUserRoute(ctx: SessionContext): Promise<boolean> {
  if (ctx.parts[2] !== "users") return false;

  // Owner access check for user management routes
  if (!isOwner(ctx.account.role)) {
    sendError(ctx.res, 403, "forbidden", "owner account required");
    return true;
  }

  const userIdOrAction = ctx.parts[3];

  // GET /api/v1/users
  if (is("GET", ctx) && !userIdOrAction) {
    const url = new URL(ctx.req.url ?? "/", "http://localhost");
    const search = url.searchParams.get("search")?.toLowerCase() || "";
    const page = parseInt(url.searchParams.get("page") || "1", 10);
    const limit = parseInt(url.searchParams.get("limit") || "10", 10);

    const allUsers = Object.values(ctx.state.accounts).map((acc) => {
      // count active/owned servers
      const serverCount = ctx.state.servers.filter((s) => s.owner === acc.id).length;
      return {
        id: `mock-${acc.id}`,
        username: acc.id,
        email: acc.email,
        role: acc.role,
        disabled: !!acc.disabled,
        email_verified: acc.emailVerified,
        server_count: serverCount,
        created_at: acc.created_at || new Date().toISOString(),
        updated_at: acc.updated_at || new Date().toISOString(),
      } as UserView;
    });

    const filtered = allUsers.filter((u) => {
      return (
        u.username.toLowerCase().includes(search) ||
        (u.email ?? "").toLowerCase().includes(search)
      );
    });

    const paginated = filtered.slice((page - 1) * limit, page * limit);

    sendJSON(ctx.res, 200, {
      users: paginated,
      total: filtered.length,
    });
    return true;
  }

  // POST /api/v1/users
  if (is("POST", ctx) && !userIdOrAction) {
    const body = await readJSON<CreateUserRequest>(ctx.req);
    const username = body.username?.trim().toLowerCase();
    if (!username) {
      sendError(ctx.res, 400, "bad_request", "username is required");
      return true;
    }
    if (ctx.state.accounts[username]) {
      sendError(ctx.res, 409, "already_exists", "username is already taken");
      return true;
    }

    const newAcc: MockAccount = {
      id: username,
      role: body.role || "user",
      email: body.email || `${username}@example.com`,
      linked: false,
      emailVerified: true,
      disabled: false,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      quota: {
        user_id: `mock-${username}`,
        max_servers: 3,
        max_cpu_milli: 4000,
        max_memory_mb: 8192,
        max_storage_gb: 50,
      },
      sessions: [],
    };

    ctx.state.accounts[username] = newAcc;

    sendJSON(ctx.res, 201, {
      id: `mock-${username}`,
      username: username,
      email: newAcc.email,
      role: newAcc.role,
      disabled: false,
      email_verified: true,
      server_count: 0,
      created_at: newAcc.created_at,
      updated_at: newAcc.updated_at,
    } as UserView);
    return true;
  }

  // Routes starting with /api/v1/users/{id}
  if (userIdOrAction) {
    const rawId = userIdOrAction;
    const accountKey = rawId.startsWith("mock-") ? rawId.substring(5) : rawId;
    const acc = ctx.state.accounts[accountKey];

    if (!acc) {
      sendError(ctx.res, 404, "not_found", "user not found");
      return true;
    }

    const subAction = ctx.parts[4];

    // GET /api/v1/users/{id}
    if (is("GET", ctx) && !subAction) {
      const serverCount = ctx.state.servers.filter((s) => s.owner === acc.id).length;
      const linked_accounts = acc.linked ? [{
        mc_uuid: MC_UUID,
        auth_source: "mojang",
        verified_at: new Date().toISOString()
      }] : [];

      const detail: UserDetail = {
        id: `mock-${acc.id}`,
        username: acc.id,
        email: acc.email,
        role: acc.role,
        disabled: !!acc.disabled,
        email_verified: acc.emailVerified,
        server_count: serverCount,
        created_at: acc.created_at || new Date().toISOString(),
        updated_at: acc.updated_at || new Date().toISOString(),
        linked_accounts,
      };
      sendJSON(ctx.res, 200, detail);
      return true;
    }

    // PATCH /api/v1/users/{id}
    if (is("PATCH", ctx) && !subAction) {
      const body = await readJSON<PatchUserRequest>(ctx.req);

      if (body.role !== undefined) {
        if (body.role !== "admin" && body.role !== "user") {
          sendError(ctx.res, 400, "bad_request", `role must be 'admin' or 'user', got ${body.role}`);
          return true;
        }
        if (ctx.account.id === acc.id && body.role !== ctx.account.role) {
          sendError(ctx.res, 403, "self_protected", "cannot change your own role");
          return true;
        }
        if (acc.role === "owner") {
          sendError(
            ctx.res,
            403,
            "owner_protected",
            "the owner account's role cannot be changed from the panel"
          );
          return true;
        }
        acc.role = body.role;
      }
      if (body.email !== undefined) acc.email = body.email;
      acc.updated_at = new Date().toISOString();

      const serverCount = ctx.state.servers.filter((s) => s.owner === acc.id).length;
      sendJSON(ctx.res, 200, {
        id: `mock-${acc.id}`,
        username: acc.id,
        email: acc.email,
        role: acc.role,
        disabled: !!acc.disabled,
        email_verified: acc.emailVerified,
        server_count: serverCount,
        created_at: acc.created_at || new Date().toISOString(),
        updated_at: acc.updated_at,
      } as UserView);
      return true;
    }

    // DELETE /api/v1/users/{id}
    if (is("DELETE", ctx) && !subAction) {
      if (ctx.account.id === acc.id) {
        sendError(ctx.res, 403, "self_protected", "cannot delete your own account");
        return true;
      }
      if (acc.role === "owner") {
        sendError(ctx.res, 403, "owner_protected", "the owner account cannot be deleted from the panel");
        return true;
      }
      const activeServers = ctx.state.servers.filter(
        (s) => s.owner === acc.id && s.phase !== "Stopped" && s.phase !== "Failed"
      );
      if (activeServers.length > 0) {
        sendError(
          ctx.res,
          409,
          "active_servers",
          "cannot delete user with active servers"
        );
        return true;
      }

      ctx.state.servers.forEach((s) => {
        if (s.owner === acc.id) {
          s.owner = null;
        }
      });

      delete ctx.state.accounts[accountKey];
      sendJSON(ctx.res, 200, { deleted: true });
      return true;
    }

    // POST /api/v1/users/{id}/disable
    if (is("POST", ctx) && subAction === "disable") {
      if (ctx.account.id === acc.id) {
        sendError(ctx.res, 403, "self_protected", "cannot disable your own account");
        return true;
      }
      const body = await readJSON<{ disabled: boolean }>(ctx.req);
      if (acc.role === "owner" && body.disabled) {
        sendError(ctx.res, 403, "owner_protected", "the owner account cannot be disabled from the panel");
        return true;
      }
      acc.disabled = !!body.disabled;
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, { id: `mock-${acc.id}`, disabled: acc.disabled });
      return true;
    }

    // GET /api/v1/users/{id}/quotas
    if (is("GET", ctx) && subAction === "quotas") {
      if (!acc.quota) {
        acc.quota = {
          user_id: `mock-${acc.id}`,
          max_servers: 3,
          max_cpu_milli: 4000,
          max_memory_mb: 8192,
          max_storage_gb: 50,
        };
      }
      sendJSON(ctx.res, 200, acc.quota);
      return true;
    }

    // PUT /api/v1/users/{id}/quotas
    if (is("PUT", ctx) && subAction === "quotas") {
      const body = await readJSON<QuotaInput>(ctx.req);
      acc.quota = {
        user_id: `mock-${acc.id}`,
        max_servers: body.max_servers,
        max_cpu_milli: body.max_cpu_milli,
        max_memory_mb: body.max_memory_mb,
        max_storage_gb: body.max_storage_gb,
      };
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, acc.quota);
      return true;
    }

    // GET /api/v1/users/{id}/sessions
    if (is("GET", ctx) && subAction === "sessions") {
      sendJSON(ctx.res, 200, { sessions: accountSessions(acc) });
      return true;
    }

    // DELETE /api/v1/users/{id}/sessions/{hash} — revoke single session
    if (is("DELETE", ctx) && subAction === "sessions" && ctx.parts[5]) {
      const hash = ctx.parts[5];
      const list = accountSessions(acc);
      if (!list.some((s) => s.token_hash === hash)) {
        sendError(ctx.res, 404, "session_not_found", "that session has already ended or does not belong to this user");
        return true;
      }
      acc.sessions = list.filter((s) => s.token_hash !== hash);
      sendJSON(ctx.res, 200, { ok: true });
      return true;
    }

    // DELETE /api/v1/users/{id}/sessions
    if (is("DELETE", ctx) && subAction === "sessions" && !ctx.parts[5]) {
      acc.sessions = [];
      sendJSON(ctx.res, 200, { ok: true });
      return true;
    }

    // POST /api/v1/users/{id}/links — manual link
    if (is("POST", ctx) && subAction === "links") {
      const body = await readJSON<{ mc_uuid: string; auth_source?: string }>(ctx.req);
      if (!body.mc_uuid) {
        sendError(ctx.res, 400, "bad_request", "mc_uuid is required");
        return true;
      }
      acc.linked = true;
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, { ok: true, mc_uuid: body.mc_uuid });
      return true;
    }

    // DELETE /api/v1/users/{id}/links/{mc_uuid} — unlink
    if (is("DELETE", ctx) && subAction === "links" && ctx.parts[5]) {
      acc.linked = false;
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, { ok: true, mc_uuid: ctx.parts[5] });
      return true;
    }
  }

  return false;
}

async function handleImageRoute(ctx: SessionContext): Promise<boolean> {
  if (ctx.parts[2] !== "images") return false;

  // GET /api/v1/images
  if (is("GET", ctx) && ctx.parts.length === 3) {
    sendJSON(ctx.res, 200, { images: ctx.state.images });
    return true;
  }

  // POST /api/v1/images (add image)
  if (is("POST", ctx) && ctx.parts.length === 3) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const body = await readJSON<{ image_ref?: string }>(ctx.req);
    const ref = body.image_ref?.trim();
    if (!ref) {
      sendError(ctx.res, 400, "bad_request", "image_ref is required");
      return true;
    }
    // Check if already exists in whitelist
    let img = ctx.state.images.find((i) => i.image_ref === ref);
    if (img) {
      img.enabled = true;
    } else {
      img = whitelistImage(ref, "external");
      ctx.state.images.unshift(img);
    }
    sendJSON(ctx.res, 201, img);
    return true;
  }

  // DELETE /api/v1/images (remove image)
  if (is("DELETE", ctx) && ctx.parts.length === 3) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const url = new URL(ctx.req.url ?? "/", "http://localhost");
    const ref = url.searchParams.get("ref");
    if (!ref) {
      sendError(ctx.res, 400, "bad_request", "ref query parameter is required");
      return true;
    }
    const idx = ctx.state.images.findIndex((i) => i.image_ref === ref);
    if (idx < 0) {
      sendError(ctx.res, 404, "not_found", "image not found");
      return true;
    }
    ctx.state.images.splice(idx, 1);
    ctx.res.statusCode = 204;
    ctx.res.end();
    return true;
  }

  // POST /api/v1/images/build (trigger build)
  if (is("POST", ctx) && ctx.parts[3] === "build" && ctx.parts.length === 4) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const body = await readJSON<{ image_ref?: string; dockerfile?: string; context_ref?: string; base_image?: string }>(ctx.req);
    if (!body.image_ref || !body.dockerfile || !body.context_ref) {
      sendError(ctx.res, 400, "bad_request", "image_ref, dockerfile, and context_ref are required");
      return true;
    }
    const newBuild: Build = {
      id: `bld-${Date.now()}`,
      image_ref: body.image_ref.trim(),
      status: "building",
      dockerfile: body.dockerfile,
      context_ref: body.context_ref.trim(),
      base_image: body.base_image?.trim(),
      requested_by: ctx.account.email,
      created_at: new Date().toISOString(),
    };
    ctx.state.builds.unshift(newBuild);

    // Mock build progression in a timeout
    setTimeout(() => {
      const b = ctx.state.builds.find((x) => x.id === newBuild.id);
      if (b && b.status === "building") {
        b.status = "succeeded";
        b.finished_at = new Date().toISOString();
        // Add to whitelist images
        if (!ctx.state.images.some((i) => i.image_ref === b.image_ref)) {
          ctx.state.images.unshift({ ...whitelistImage(b.image_ref, "built"), build_id: b.id });
        }
      }
    }, 15000); // Succeeded after 15 seconds

    sendJSON(ctx.res, 202, newBuild);
    return true;
  }

  // GET /api/v1/images/build (list builds)
  if (is("GET", ctx) && ctx.parts[3] === "build" && ctx.parts.length === 4) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    // Same page and match as build.PGStore.ListBuilds: newest first, a query hits
    // the id or status exactly or any part of the ref, and rows carry no Dockerfile.
    const url = new URL(ctx.req.url ?? "/", "http://localhost");
    const q = (url.searchParams.get("query") ?? "").trim().toLowerCase();
    const limit = Math.min(Number(url.searchParams.get("limit")) || 20, 100);
    const offset = Math.max(Number(url.searchParams.get("offset")) || 0, 0);
    const matched = ctx.state.builds
      .filter((b) => !q || b.id.toLowerCase() === q || b.status === q || b.image_ref.toLowerCase().includes(q))
      .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
    sendJSON(ctx.res, 200, {
      builds: matched.slice(offset, offset + limit).map(({ dockerfile: _, ...b }) => b),
      total: matched.length,
    });
    return true;
  }

  // GET /api/v1/images/build/{id} (get build)
  if (is("GET", ctx) && ctx.parts[3] === "build" && ctx.parts[4] && ctx.parts.length === 5) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const build = ctx.state.builds.find((b) => b.id === ctx.parts[4]);
    if (!build) {
      sendError(ctx.res, 404, "not_found", "build not found");
      return true;
    }
    sendJSON(ctx.res, 200, build);
    return true;
  }

  // GET /api/v1/images/build/{id}/scan, /scan/report, /sbom (the kept scan). The
  // seeded builds bld-1 (passed) and bld-2 (blocked) have one; the older history
  // ran before builds kept their scans.
  if (
    is("GET", ctx) && ctx.parts[3] === "build" && ctx.parts[4] &&
    ((ctx.parts[5] === "scan" && (ctx.parts.length === 6 || (ctx.parts[6] === "report" && ctx.parts.length === 7))) ||
      (ctx.parts[5] === "sbom" && ctx.parts.length === 6))
  ) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const build = ctx.state.builds.find((b) => b.id === ctx.parts[4]);
    const scan = build ? mockScan(build) : null;
    if (!build || !scan) {
      sendError(ctx.res, 404, "scan_not_found",
        "this build has no scan: it has not reached the scan step, or it ran before builds kept their scans");
      return true;
    }
    if (ctx.parts.length === 6 && ctx.parts[5] === "scan") {
      sendJSON(ctx.res, 200, scan);
      return true;
    }
    const sbom = ctx.parts[5] === "sbom";
    if (sbom && !scan.has_sbom) {
      sendError(ctx.res, 404, "scan_document_not_kept",
        "this build's scan kept no SBOM: it was too large to keep, or the step that writes it failed");
      return true;
    }
    ctx.res.statusCode = 200;
    ctx.res.setHeader("Content-Type", sbom ? "application/vnd.cyclonedx+json" : "application/json");
    ctx.res.setHeader("Content-Disposition", `attachment; filename="${build.id}${sbom ? ".cdx.json" : "-trivy.json"}"`);
    ctx.res.end(JSON.stringify(sbom
      ? { bomFormat: "CycloneDX", specVersion: "1.6", components: [] }
      : { SchemaVersion: 2, ArtifactName: build.image_ref, Results: [] }, null, 2));
    return true;
  }

  // POST /api/v1/images/build/{id}/cancel (cancel build)
  if (is("POST", ctx) && ctx.parts[3] === "build" && ctx.parts[5] === "cancel" && ctx.parts.length === 6) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const buildID = ctx.parts[4];
    const build = ctx.state.builds.find((b) => b.id === buildID);
    if (!build) {
      sendError(ctx.res, 404, "not_found", "build not found");
      return true;
    }
    if (build.status === "succeeded" || build.status === "failed" || build.status === "cancelled") {
      sendError(ctx.res, 409, "already_terminal", "build already terminal");
      return true;
    }
    build.status = "cancelled";
    build.finished_at = new Date().toISOString();
    sendJSON(ctx.res, 200, build);
    return true;
  }

  // GET /api/v1/images/build/{id}/logs (SSE logs stream)
  if (is("GET", ctx) && ctx.parts[3] === "build" && ctx.parts[5] === "logs" && ctx.parts.length === 6) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const buildID = ctx.parts[4];
    const build = ctx.state.builds.find((b) => b.id === buildID);
    if (!build) {
      sendError(ctx.res, 404, "not_found", "build not found");
      return true;
    }
    streamBuildLogs(ctx.req, ctx.res, buildID);
    return true;
  }

  return false;
}

// The same page as submit.PGStore.PageSubmissions: counts cover the whole scope,
// status and query (id, submitter or name, any part) narrow the rows, newest first.
function submissionPageOf(ctx: SessionContext, scope: Submission[]) {
  const url = new URL(ctx.req.url ?? "/", "http://localhost");
  const status = url.searchParams.get("status") ?? "";
  const q = (url.searchParams.get("query") ?? "").trim().toLowerCase();
  const limit = Math.min(Number(url.searchParams.get("limit")) || 20, 100);
  const offset = Math.max(Number(url.searchParams.get("offset")) || 0, 0);
  const counts = { pending_review: 0, approved: 0, rejected: 0 };
  for (const s of scope) counts[s.status] += 1;
  const matched = scope
    .filter((s) => !status || s.status === status)
    .filter((s) => !q || [s.id, s.submitted_by, s.display_name].some((f) => f.toLowerCase().includes(q)))
    .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
  return { submissions: matched.slice(offset, offset + limit), total: matched.length, counts };
}

async function handleSubmissionRoute(ctx: SessionContext): Promise<boolean> {
  const isAdminSubmissions = ctx.parts[2] === "submissions";
  const isMeSubmissions = ctx.parts[2] === "me" && ctx.parts[3] === "submissions";
  if (!isAdminSubmissions && !isMeSubmissions) return false;

  // GET /api/v1/me/submissions
  if (isMeSubmissions && is("GET", ctx) && ctx.parts.length === 4) {
    const userSubs = ctx.state.submissions.filter((s) => s.submitted_by === ctx.account.email);
    sendJSON(ctx.res, 200, submissionPageOf(ctx, userSubs));
    return true;
  }

  // GET /api/v1/me/submissions/limits — the Cloudflare-edge default.
  if (isMeSubmissions && is("GET", ctx) && ctx.parts.length === 5 && ctx.parts[4] === "limits") {
    sendJSON(ctx.res, 200, { max_context_bytes: 95 * 1024 * 1024 });
    return true;
  }
  // POST /api/v1/me/submissions
  if (isMeSubmissions && is("POST", ctx) && ctx.parts.length === 4) {
    const body = await readJSON<{ display_name?: string }>(ctx.req);
    const displayName = body.display_name?.trim();
    if (!displayName) {
      sendError(ctx.res, 400, "bad_request", "display_name is required");
      return true;
    }
    const id = `sub-${Date.now()}`;
    const newSub: Submission = {
      id,
      submitted_by: ctx.account.email,
      display_name: displayName,
      context_ref: `minio/contexts/${id}/context.tar.gz`,
      status: "pending_review",
      created_at: new Date().toISOString(),
    };
    ctx.state.submissions.unshift(newSub);
    sendJSON(ctx.res, 201, newSub);
    return true;
  }

  // POST /api/v1/me/submissions/{id}/context
  if (isMeSubmissions && is("POST", ctx) && ctx.parts[5] === "context" && ctx.parts.length === 6) {
    const id = ctx.parts[4];
    const sub = ctx.state.submissions.find((s) => s.id === id);
    if (!sub) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    if (sub.submitted_by !== ctx.account.email) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    if (sub.status !== "pending_review") {
      sendError(ctx.res, 409, "already_reviewed", "submission has already been reviewed");
      return true;
    }

    const chunks: Buffer[] = [];
    for await (const chunk of ctx.req) {
      chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    }
    const blob = Buffer.concat(chunks);
    contextBlobs.set(id, blob);
    sub.context_sha256 = sha256Hex(blob);

    sendJSON(ctx.res, 200, sub);
    return true;
  }

  // GET /api/v1/submissions/{id}/context — the reviewer's download, carrying the
  // digest the approval must name.
  if (isAdminSubmissions && is("GET", ctx) && ctx.parts[4] === "context" && ctx.parts.length === 5) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const id = ctx.parts[3];
    const sub = ctx.state.submissions.find((s) => s.id === id);
    if (!sub || !sub.context_sha256) {
      sendError(ctx.res, 404, "not_found", "no context uploaded for this submission");
      return true;
    }
    const blob = contextBlob(id);
    ctx.res.writeHead(200, {
      "Content-Type": "application/gzip",
      "Content-Disposition": `attachment; filename="${id}-context.tar.gz"`,
      "X-Felis-Context-Sha256": sha256Hex(blob),
    });
    ctx.res.end(blob);
    return true;
  }

  // GET /api/v1/submissions
  if (isAdminSubmissions && is("GET", ctx) && ctx.parts.length === 3) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    sendJSON(ctx.res, 200, submissionPageOf(ctx, ctx.state.submissions));
    return true;
  }

  // POST /api/v1/submissions/{id}/approve
  if (isAdminSubmissions && is("POST", ctx) && ctx.parts[4] === "approve" && ctx.parts.length === 5) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const id = ctx.parts[3];
    const sub = ctx.state.submissions.find((s) => s.id === id);
    if (!sub) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    if (sub.status !== "pending_review") {
      sendError(ctx.res, 409, "already_reviewed", "submission already reviewed");
      return true;
    }
    const { expected_digest: expected } = await readJSON<{ expected_digest?: string }>(ctx.req);
    if (!/^[0-9a-f]{64}$/.test(expected?.trim().toLowerCase() ?? "")) {
      sendError(ctx.res, 400, "bad_request", "expected_digest must be the sha256 of the context you reviewed (64 hex characters)");
      return true;
    }
    if (!sub.context_sha256) {
      sendError(ctx.res, 400, "bad_request", "this context was uploaded before digests were recorded; the submitter must upload it again");
      return true;
    }
    if (expected!.trim().toLowerCase() !== sub.context_sha256) {
      sendError(ctx.res, 409, "context_changed", "the build context was uploaded again after it was reviewed; review the new upload before approving");
      return true;
    }

    sub.status = "approved";
    sub.reviewed_by = ctx.account.email;
    sub.reviewed_at = new Date().toISOString();
    sub.image_ref = `registry.felis.svc:5000/user-uploads/${id}:latest`;

    // Trigger mock build
    const newBuild: Build = {
      id: `bld-${Date.now()}`,
      image_ref: sub.image_ref,
      status: "building",
      dockerfile: `# felis user-modpack submission ${id}\n# The executed Dockerfile is provided by the uploaded build context:\n#   ${sub.context_ref}\n`,
      context_ref: sub.context_ref,
      requested_by: ctx.account.email,
      created_at: new Date().toISOString(),
    };
    ctx.state.builds.unshift(newBuild);
    sub.build_id = newBuild.id;

    // Succeeded after 15 seconds
    setTimeout(() => {
      const b = ctx.state.builds.find((x) => x.id === newBuild.id);
      if (b && b.status === "building") {
        b.status = "succeeded";
        b.finished_at = new Date().toISOString();
        if (!ctx.state.images.some((i) => i.image_ref === b.image_ref)) {
          ctx.state.images.unshift({ ...whitelistImage(b.image_ref, "built"), build_id: b.id });
        }
      }
    }, 15000);

    sendJSON(ctx.res, 200, sub);
    return true;
  }

  // POST /api/v1/submissions/{id}/reject
  if (isAdminSubmissions && is("POST", ctx) && ctx.parts[4] === "reject" && ctx.parts.length === 5) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    const id = ctx.parts[3];
    const sub = ctx.state.submissions.find((s) => s.id === id);
    if (!sub) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    if (sub.status !== "pending_review") {
      sendError(ctx.res, 409, "already_reviewed", "submission already reviewed");
      return true;
    }

    const body = await readJSON<{ reason?: string }>(ctx.req);
    const reason = body.reason?.trim();
    if (!reason) {
      sendError(ctx.res, 400, "bad_request", "reason is required");
      return true;
    }

    sub.status = "rejected";
    sub.reviewed_by = ctx.account.email;
    sub.reviewed_at = new Date().toISOString();
    sub.reject_reason = reason;

    sendJSON(ctx.res, 200, sub);
    return true;
  }

  return false;
}

function streamBuildLogs(
  req: IncomingMessage,
  res: ServerResponse,
  buildID: string
): void {
  const lines = [
    `[INFO] [Kaniko] Starting build for ID: ${buildID}`,
    "[INFO] [Kaniko] Pulling base image library/postgres:15",
    "[INFO] [Kaniko] Successfully pulled base image",
    "[INFO] [Kaniko] Executing: RUN echo 'setup'",
    "[INFO] [Kaniko] Pushing image to registry.felis.svc:5000",
    "[INFO] [Trivy] Starting security scan...",
    "[INFO] [Trivy] Scanning registry.felis.svc:5000/image",
    "[INFO] [Trivy] No critical vulnerabilities found. Scan PASSED.",
    `[INFO] [System] Build succeeded for ${buildID}`,
  ];
  let i = 0;

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
  });
  res.write(": connected\n\n");
  // A modpack boot floods the console; the burst exercises the frame batching.
  for (let n = 0; n < 400; n++) {
    res.write(`data: [11:59:59] [main/INFO]: Loading mod ${n + 1}/400 (mock-mod-${n})\n\n`);
  }

  const timer = setInterval(() => {
    if (i < lines.length) {
      res.write(`data: ${lines[i]}\n\n`);
      i++;
    } else {
      clearInterval(timer);
    }
  }, 1000);

  req.on("close", () => clearInterval(timer));
}

async function createServerRoute(ctx: SessionContext): Promise<void> {
  if (!isAdmin(ctx.account.role)) {
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

async function handleServerRoute(ctx: SessionContext): Promise<boolean> {
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
    sendJSON(ctx.res, 200, statusView(serverInfo, ctx.account));
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
  if (is("POST", ctx) && ctx.parts[4] === "restore-backup") {
    return await handleRestoreBackupMock(ctx, serverInfo);
  }
  if (is("POST", ctx) && ctx.parts[4] === "backup") {
    handleBackupNowMock(ctx, serverInfo);
    return true;
  }
  if (is("GET", ctx) && ctx.parts[4] === "jobs") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    sendJSON(ctx.res, 200, { server: serverInfo.name, jobs: ctx.state.jobs[serverInfo.name] ?? [] });
    return true;
  }
  if (ctx.parts[4] === "access") {
    return handleAccessMock(ctx, serverInfo);
  }

  return false;
}

// handleRestoreBackupMock mirrors the backend's restore authorization order
// (handlers_backups.go): owner-or-admin → specific backup by id or latest present
// backup else 404 no_backup → non-admin former-owner match → stopped gate else
// 409 not_stopped → 202.
async function handleRestoreBackupMock(ctx: SessionContext, serverInfo: MockServer): Promise<boolean> {
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return true;
  }
  let backupId: string | undefined;
  let safety = true;
  try {
    const body = await readJSON<{ backup_id?: string; safety_snapshot?: boolean }>(ctx.req);
    backupId = body.backup_id;
    safety = body.safety_snapshot !== false;
  } catch (e) {
    // Ignore if body is empty or unparsable
  }

  let backup: any = null;
  if (backupId) {
    backup = ctx.state.backups.find((b) => b.id === backupId && b.server_name === serverInfo.name && b.status === "present");
    if (!backup) {
      sendError(ctx.res, 404, "no_backup", "no restorable backup exists for this server");
      return true;
    }
  } else {
    const latest = ctx.state.backups
      .filter((b) => b.server_name === serverInfo.name && b.status === "present")
      .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))[0];
    if (!latest) {
      sendError(ctx.res, 404, "no_backup", "no restorable backup exists for this server");
      return true;
    }
    backup = latest;
  }

  // Non-admins may restore only a world they formerly owned (spec §466).
  if (!isAdmin(ctx.account.role) && backup.former_owner !== ctx.account.id) {
    sendError(ctx.res, 403, "forbidden", "not the former owner of this world");
    return true;
  }
  // The world PVC must be free — a running/starting server still holds it.
  if (serverInfo.phase !== "Stopped") {
    sendError(ctx.res, 409, "not_stopped", "stop the server before restoring a backup");
    return true;
  }
  const now = new Date().toISOString();
  const done = { state: "succeeded", started_at: now, finished_at: now };
  const jobs: ServerJob[] = [{ name: `restore-${serverInfo.name}-${Date.now()}`, kind: "restore", ...done }];
  if (safety) {
    ctx.state.backups.unshift(backup(serverInfo.name, 0, 0.9 * GiB, ctx.account.id, "pre_restore"));
    jobs.push({
      name: `backup-${serverInfo.name}-${Date.now()}`,
      kind: "backup",
      ...done,
      then_restore: "started",
      restore_backup_id: backup.id,
    });
  }
  ctx.state.jobs[serverInfo.name] = [...jobs, ...(ctx.state.jobs[serverInfo.name] ?? [])];
  sendJSON(ctx.res, 202, {
    name: serverInfo.name,
    status: "restoring",
    backup_id: backup.id,
    safety_snapshot: safety,
  });
  return true;
}

// handleBackupNowMock mirrors POST /servers/{name}/backup: owner-or-admin, then
// the stopped gate (the world volume is RWO), then 202. The Job finishes at once.
function handleBackupNowMock(ctx: SessionContext, serverInfo: MockServer): void {
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return;
  }
  if (serverInfo.phase !== "Stopped") {
    sendError(ctx.res, 409, "not_stopped", "stop the server before backing it up");
    return;
  }
  const now = new Date().toISOString();
  ctx.state.backups.unshift(backup(serverInfo.name, 0, 0.9 * GiB, ctx.account.id, "manual"));
  const job: ServerJob = {
    name: `backup-${serverInfo.name}-${Date.now()}`,
    kind: "backup",
    state: "succeeded",
    started_at: now,
    finished_at: now,
  };
  ctx.state.jobs[serverInfo.name] = [job, ...(ctx.state.jobs[serverInfo.name] ?? [])];
  sendJSON(ctx.res, 202, { name: serverInfo.name, status: "backing_up" });
}

function accessFor(state: MockState, name: string): AccessState {
  let entry = state.access[name];
  if (!entry) {
    entry = { whitelist: [], banned: [], online: [], luckperms: {} };
    state.access[name] = entry;
  }
  return entry;
}

function whitelistOutput(players: string[]): string {
  if (players.length === 0) return "There are no whitelisted players";
  return `There are ${players.length} whitelisted player(s): ${players.join(", ")}`;
}

function listOutput(online: string[], max: number): string {
  const head = `There are ${online.length} of a max of ${max} players online:`;
  return online.length === 0 ? head : `${head} ${online.join(", ")}`;
}

// banlistOutput reproduces vanilla's multiline "banlist" reply: a header line then
// one "<name> was banned by <source>: <reason>" line per ban. The panel's parser
// (parseBanlistOutput) keys on the " was banned by " marker, so this exercises the
// real shape — header + reasons that carry their own colons and spaces — end to end.
function banlistOutput(banned: string[]): string {
  if (banned.length === 0) return "There are no bans.";
  const head = `There are ${banned.length} ban(s):`;
  const lines = banned.map((p) => `${p} was banned by Server: Banned by an operator.`);
  return [head, ...lines].join("\n");
}

// handleAccessMock mirrors issueAccessCommand's two gates — owner/admin AND the
// server being Running (RCON) — before dispatching the whitelist/ban routes. The GET
// whitelist read is behind the SAME Running gate as the writes, exactly as the real
// readiness check covers it (409 not_running on a cold server).
function handleAccessMock(ctx: SessionContext, serverInfo: MockServer): boolean {
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return true;
  }
  if (serverInfo.phase !== "Running") {
    sendError(
      ctx.res,
      409,
      "not_running",
      "server is not running; wake it before managing access",
    );
    return true;
  }

  const sub = ctx.parts[5];
  const access = accessFor(ctx.state, serverInfo.name);

  if (is("GET", ctx) && sub === "luckperms") {
    const player = ctx.parts[6];
    if (!player || !PLAYER_NAME.test(player)) {
      sendError(ctx.res, 400, "bad_request", "invalid player name");
      return true;
    }
    const lpData = access.luckperms[player] || { groups: ["default"], permissions: [] };
    sendJSON(ctx.res, 200, {
      player,
      groups: lpData.groups,
      permissions: lpData.permissions,
    });
    return true;
  }

  if (is("GET", ctx) && sub === "whitelist") {
    sendJSON(ctx.res, 200, {
      name: serverInfo.name,
      players: [...access.whitelist],
      output: whitelistOutput(access.whitelist),
    });
    return true;
  }
  if (is("POST", ctx) && sub === "whitelist") {
    void handleListMutation(ctx, serverInfo, access, "whitelist");
    return true;
  }
  if (is("GET", ctx) && sub === "players") {
    const max = serverInfo.playersMax ?? 0;
    sendJSON(ctx.res, 200, {
      name: serverInfo.name,
      online: access.online.length,
      max,
      players: [...access.online],
      output: listOutput(access.online, max),
    });
    return true;
  }
  if (is("POST", ctx) && sub === "kick") {
    void handleKickMock(ctx, serverInfo, access);
    return true;
  }
  if (is("GET", ctx) && sub === "ban") {
    sendJSON(ctx.res, 200, {
      name: serverInfo.name,
      players: [...access.banned],
      output: banlistOutput(access.banned),
    });
    return true;
  }
  if (is("POST", ctx) && sub === "ban") {
    void handleListMutation(ctx, serverInfo, access, "ban");
    return true;
  }
  if (is("POST", ctx) && sub === "group") {
    void handleGroupMock(ctx, serverInfo);
    return true;
  }
  if (is("POST", ctx) && sub === "permission") {
    void handlePermissionMock(ctx, serverInfo);
    return true;
  }

  return false;
}

// handleKickMock backs POST .../access/kick: charset-validate the player, drop them
// from the online roster (so a reload reflects it), and echo {name, player, output}.
async function handleKickMock(
  ctx: SessionContext,
  serverInfo: MockServer,
  access: AccessState,
): Promise<void> {
  const body = await readJSON<{ player?: string }>(ctx.req);
  const player = body.player?.trim() ?? "";
  if (!PLAYER_NAME.test(player)) {
    sendError(ctx.res, 400, "bad_request", "invalid player name");
    return;
  }
  const i = access.online.indexOf(player);
  if (i >= 0) access.online.splice(i, 1);
  sendJSON(ctx.res, 200, {
    name: serverInfo.name,
    player,
    output: `[mock] kick ${player}`,
  });
}

async function handleGroupMock(
  ctx: SessionContext,
  serverInfo: MockServer,
): Promise<void> {
  const body = await readJSON<{ action?: string; player?: string; group?: string }>(ctx.req);
  const player = body.player?.trim() ?? "";
  const group = body.group?.trim() ?? "";
  const action = body.action?.trim() ?? "";

  if (!PLAYER_NAME.test(player)) {
    sendError(ctx.res, 400, "bad_request", "invalid player name");
    return;
  }
  if (!LP_CTX.test(group)) {
    sendError(ctx.res, 400, "bad_request", "invalid group name");
    return;
  }
  if (action !== "add" && action !== "remove") {
    sendError(ctx.res, 400, "bad_request", "invalid action");
    return;
  }

  const access = accessFor(ctx.state, serverInfo.name);
  if (!access.luckperms[player]) {
    access.luckperms[player] = { groups: ["default"], permissions: [] };
  }
  const lp = access.luckperms[player];
  if (action === "add") {
    if (!lp.groups.includes(group)) lp.groups.push(group);
  } else if (action === "remove") {
    lp.groups = lp.groups.filter((g) => g !== group);
  }

  sendJSON(ctx.res, 200, {
    name: serverInfo.name,
    action,
    player,
    group,
    output: `[mock-luckperms] Added parent group '${group}' for user ${player} in context global`,
  });
}

async function handlePermissionMock(
  ctx: SessionContext,
  serverInfo: MockServer,
): Promise<void> {
  const body = await readJSON<{
    action?: string;
    player?: string;
    node?: string;
    value?: boolean;
    world?: string;
  }>(ctx.req);
  const player = body.player?.trim() ?? "";
  const node = body.node?.trim() ?? "";
  const action = body.action?.trim() ?? "";
  const value = body.value ?? true;
  const world = body.world?.trim() ?? "";

  if (!PLAYER_NAME.test(player)) {
    sendError(ctx.res, 400, "bad_request", "invalid player name");
    return;
  }
  if (!LP_NODE.test(node)) {
    sendError(ctx.res, 400, "bad_request", "invalid permission node");
    return;
  }
  if (world !== "" && !LP_CTX.test(world)) {
    sendError(ctx.res, 400, "bad_request", "invalid world context");
    return;
  }
  if (action !== "set" && action !== "unset") {
    sendError(ctx.res, 400, "bad_request", "invalid action");
    return;
  }

  const access = accessFor(ctx.state, serverInfo.name);
  if (!access.luckperms[player]) {
    access.luckperms[player] = { groups: ["default"], permissions: [] };
  }
  const lp = access.luckperms[player];
  if (action === "set") {
    lp.permissions = lp.permissions.filter((p) => !(p.node === node && p.world === world));
    lp.permissions.push({ node, value, world: world || undefined });
  } else if (action === "unset") {
    lp.permissions = lp.permissions.filter((p) => !(p.node === node && p.world === world));
  }

  const worldSuffix = world ? ` (world=${world})` : "";
  const outputMsg = action === "set"
    ? `[mock-luckperms] Set permission ${node} to ${value} for ${player}${worldSuffix}`
    : `[mock-luckperms] Unset permission ${node} for ${player}${worldSuffix}`;

  sendJSON(ctx.res, 200, {
    name: serverInfo.name,
    action,
    player,
    node,
    value,
    world: world || undefined,
    output: outputMsg,
  });
}

// handleListMutation backs both POST .../access/whitelist (add|remove) and
// POST .../access/ban (ban|pardon): the same structured {action, player} shape with
// a charset-validated player, echoing back {name, action, player, output}.
async function handleListMutation(
  ctx: SessionContext,
  serverInfo: MockServer,
  access: AccessState,
  kind: "whitelist" | "ban",
): Promise<void> {
  const body = await readJSON<{ action?: string; player?: string }>(ctx.req);
  const player = body.player?.trim() ?? "";
  if (!PLAYER_NAME.test(player)) {
    sendError(ctx.res, 400, "bad_request", "invalid player name");
    return;
  }

  const list = kind === "whitelist" ? access.whitelist : access.banned;
  const addAction = kind === "whitelist" ? "add" : "ban";
  const removeAction = kind === "whitelist" ? "remove" : "pardon";

  if (body.action === addAction) {
    if (!list.includes(player)) list.push(player);
    // A ban also removes the player from the live server, so drop them from the
    // online roster too — the real "ban" kicks them as a side effect.
    if (kind === "ban") {
      const oi = access.online.indexOf(player);
      if (oi >= 0) access.online.splice(oi, 1);
    }
  } else if (body.action === removeAction) {
    const i = list.indexOf(player);
    if (i >= 0) list.splice(i, 1);
  } else {
    sendError(ctx.res, 400, "bad_request", "unknown action");
    return;
  }

  sendJSON(ctx.res, 200, {
    name: serverInfo.name,
    action: body.action,
    player,
    output: `[mock] ${body.action} ${player}`,
  });
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
    "[12:00:05] [Server thread/INFO]: [Essentials] §aReady§r — §6§lwelcome§r to §x§5§5§c§c§f§fFelis",
    "[12:00:06] [Server thread/WARN]: \x1b[33mTPS dropped to 18.2\x1b[0m",
  ];
  let i = 0;

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
  });
  res.write(": connected\n\n");
  // A modpack boot floods the console; the burst exercises the frame batching.
  for (let n = 0; n < 400; n++) {
    res.write(`data: [11:59:59] [main/INFO]: Loading mod ${n + 1}/400 (mock-mod-${n})\n\n`);
  }

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
          sendJSON(res, 200, {
            apiBase: API_BASE,
            rootDomain: ROOT_DOMAIN,
            build: { version: "v0.4.0+g1a2b3c4", release: "v0.4.0", commit: "1a2b3c4", dev: true },
          });
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
