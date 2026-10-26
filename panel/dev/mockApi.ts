import { createHash } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { isIP } from "node:net";
import { gzipSync } from "node:zlib";
import { mockArchive, mockZip } from "./fileArchives";
import { getYamlScalar, readYamlConfig, setYamlScalar, YamlConfigParseError } from "../src/components/files/yamlConfig";
import { PAPER_GLOBAL_FIELDS } from "../src/components/files/paperGlobalFields";
import { configProblem, type ConfigFieldSpec } from "../src/components/files/configFields";
import { memoryLimit } from "../src/lib/resourceUsage";
import type { Plugin } from "vite";
import type {
  AuthSourceConfig,
  AuthSourcesSettings,
  WakePolicySettings,
  MailSettings,
  MailSettingsRequest,
  ArchivePolicySettings,
  HeartbeatSettings,
  FailoverPolicySettings,
  EntryPolicySettings,
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
  ServerNotify,
  ServerStatus,
  ServerExtraPort,
  ServerStartupSettings,
  StartupSettingsPatch,
  WhitelistImage,
  Submission,
  UserView,
  UserDetail,
  CreateUserRequest,
  PatchUserRequest,
  QuotaView,
  QuotaInput,
  SessionView,
  HostOverview,
  NodeControlRequest,
  NodeControlTask,
  SelfCreateRequest,
  SelfServiceQuotaSettings,
  ServerQuota,
  ServerSoftware,
  SoftwareBuild,
  SoftwareRequest,
  SoftwareType,
  FileOp,
  ExtensionMetadata,
} from "../src/lib/types";

type AccountID = string;
type Role = "admin" | "user" | "owner";
type Method = "GET" | "POST" | "DELETE" | "PATCH" | "PUT";
type CreateError =
  | "bad_request"
  | "already_exists"
  | "subdomain_taken"
  | "image_not_whitelisted"
  | "eula_required"
  | SoftwareError;

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
  quota?: Omit<QuotaView, "custom">; // the default quota applies when absent
  sessions?: SessionView[];
  reauthUntil?: number; // ms epoch until which the session counts as re-authenticated
  pendingEmail?: string; // address the email-change code was last sent to
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
  online: string[]; // stands in for the RCON "list" roster; kick and ban remove entries
  luckperms: Record<string, PlayerLuckPerms>;
}

interface MockState {
  accounts: Record<AccountID, MockAccount>;
  servers: MockServer[];
  startupSettings: Map<string, ServerStartupSettings>;
  images: WhitelistImage[];
  access: Record<string, AccessState>;
  backups: BackupView[];
  jobs: Record<string, ServerJob[]>; // backup/restore jobs by server, newest first
  builds: Build[];
  passkeys: Record<AccountID, { id: string; name: string; created_at: string }[]>;
  submissions: Submission[];
  updateWindow: { start: string | null; end: string | null };
  authSources: AuthSourcesSettings;
  wakePolicy: WakePolicySettings;
  mailSettings: MailSettings;
  mailPassword: string;
  archivePolicy: ArchivePolicySettings;
  heartbeat: HeartbeatSettings;
  failoverPolicy: FailoverPolicySettings;
  entryPolicy: EntryPolicySettings;
  selfService: SelfServiceQuotaSettings;
  host: MockHost;
  files: Record<string, MockTree>; // per-server world volume, seeded on first visit
  fileOps: Record<string, FileOp[]>;
  fileExports: Record<string, { owner: string; filename: string; data: Buffer; contentType: string; expires: number; spent?: boolean }>;
}

// MockHost stands in for the node-control service; its tasks finish at once.
interface MockHost {
  tasks: NodeControlTask[];
  quietUntil?: string;
  settings: HostOverview["settings"];
  offsite: HostOverview["offsite"];
  connection: HostOverview["connection"];
  storage: HostOverview["storage"];
}

// Keep in sync with mcNameRe in handlers_access.go.
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

const GiB = 1024 ** 3;
const DAY_MS = 86_400_000;

// Uploaded build contexts by submission id; seeded submissions get a stand-in archive.
const contextBlobs = new Map<string, Buffer>();

// Staged chunked uploads by submission id; the small part cap forces multi-part uploads.
const stagedParts = new Map<string, Buffer>();
const MOCK_PART_MAX = 256 * 1024;
const MOCK_CONTEXT_MAX = 1024 * 1024 * 1024;

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
    backup("survival", 88, 2.1 * GiB, "owner"), // ~2 days from expiry
    backup("modded", 12, 0.6 * GiB, "owner"),
    backup("lobby", 2, 0.8 * GiB, "linked", "manual"),
    backup("lobby", 30, 0.7 * GiB, "linked"),
  ];
}

function initialState(): MockState {
  const state: MockState = {
    startupSettings: new Map([
      ["survival", {
        java_flags: ["-Dmode=old"], args: ["--universe", "world with spaces", "", "first line\nsecond line"],
        env: [
          { name: "FELIS_MANAGED_VALUE", value: "mock-managed-value" },
          { name: "JAVA_HOME", value: "/opt/felis/jre" },
          { name: "PLUGIN_MODE", value: "mock fake value" },
          { name: "PLUGIN_TEXT", value: "first line\nsecond line" },
        ],
        stop_commands: ["say mock stopping"],
      }],
    ]),
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
        memory: "4Gi",
        javaMemory: "3072M",
        storageSize: "20Gi",
        // tag@digest, as felis-api stores it
        image:
          "registry.felis.svc:5000/paper-1.21:demo@sha256:3b637d68843f53750a2e3c2d29c0346b6f81ffd22db2902b80bd5048ee04cc09",
        recentCrashes: 1,
        lastCrash: { at: minutesAgo(18), reason: "Error", exitCode: 1 },
        events: [
          { seq: 1, type: "HostRestarted", at: minutesAgo(26 * 60) },
          { seq: 2, type: "Started", at: minutesAgo(26 * 60 - 2) },
          { seq: 3, type: "Crashed", at: minutesAgo(18), reason: "Error", exitCode: 1, crashes: 1, limit: 3 },
          { seq: 4, type: "Started", at: minutesAgo(17) },
        ],
      }),
      server("lobby", "Hub Lobby", "Running", "linked", {
        playersOnline: 28,
        playersMax: 60,
        autostartPolicy: "public",
        memory: "2Gi",
        javaMemory: "1536M",
        storageSize: "10Gi",
        image: runtimeImage(21),
        software: { type: "paper", minecraftVersion: "1.21.4", build: "232", java: 21, forwarding: "velocity" },
      }),
      server("creative", "Creative Lab", "Stopped", "user", {
        autostartPolicy: "public",
        playersMax: 16,
        recentCrashes: 4,
        lastCrash: { at: minutesAgo(42), reason: "OOMKilled", exitCode: 137 },
        autoStop: { reason: "CrashLimit", at: minutesAgo(42) },
        crashStopped: true,
        events: [
          { seq: 7, type: "Crashed", at: minutesAgo(50), reason: "OOMKilled", exitCode: 137, crashes: 1, limit: 3 },
          { seq: 8, type: "Crashed", at: minutesAgo(48), reason: "OOMKilled", exitCode: 137, crashes: 2, limit: 3 },
          { seq: 9, type: "Crashed", at: minutesAgo(45), reason: "OOMKilled", exitCode: 137, crashes: 3, limit: 3 },
          { seq: 10, type: "Crashed", at: minutesAgo(42), reason: "OOMKilled", exitCode: 137, crashes: 4, limit: 3, stopping: true },
          { seq: 11, type: "Stopped", at: minutesAgo(41), reason: "CrashLimit" },
        ],
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
      server("login", "Login space", "Stopped", null, {
        autostartPolicy: "public", idleStopSeconds: 0, reaperExempt: true,
        image: "registry.felis.svc:5000/felis/limbo:demo", memory: "512Mi", storageSize: "1Gi",
      }),
      ...generatedServers(),
    ],
    access: {
      // 25 whitelisted names: past one page (10) and the search threshold (8)
      survival: {
        whitelist: [
          "mock_player", "test_player", "Notch", "jeb_", "Dinnerbone",
          "Grumm", "Steve", "Alex", "Herobrine", "Technoblade",
          "Dream", "GeorgeNotFound", "Sapnap", "BadBoyHalo", "Skeppy",
          "Tommyinnit", "Tubbo", "Ranboo", "Wilbur_Soot", "Philza",
          "Captain_Puffy", "Nihachu", "Fundy", "Quackity", "Karl_Jacobs",
        ],
        // 12 banned names, disjoint from the online roster
        banned: [
          "Griefer_99", "tnt_troll", "hack_client_x", "spam_bot_01", "lava_caster",
          "dupe_glitcher", "griefKing", "nukebot", "AFK_farmer", "chat_spammer",
          "xray_cheater", "fly_hacker",
        ],
        // 12 online, matching playersOnline; includes non-whitelisted names for kick and ban
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
      {
        id: "bld-3",
        image_ref: "registry.felis.svc:5000/user-uploads/sub-9:latest",
        status: "building",
        requested_by: "admin2@mock.felis.local",
        created_at: new Date(Date.now() - 300000).toISOString(),
      },
      // enough history for a second page
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
    entryPolicy: { mode: "domain", defaultServer: "", requireAccountLink: true, offlineAction: "wake", waitingSpace: "lobby", fallbackServer: "", revision: "initial" },
    wakePolicy: { maxRunningServers: 0, wakeCooldownSeconds: 30, revision: "initial", managed: false },
    mailSettings: { source: "host", host: "smtp.mock.felis.local", port: 587, from: "noreply@mock.felis.local", username: "noreply@mock.felis.local", password_set: true, require_tls: true, max_per_hour: 0, revision: "" },
    mailPassword: "mock",
    archivePolicy: hostArchivePolicy(),
    heartbeat: { set: false, display: "", revision: "" },
    failoverPolicy: { automatic: false, afterMinutes: 15, replicaMinutes: 60, revision: "", managed: false },
    selfService: { enabled: false, max_servers: null, max_cpu_milli: null, max_memory_mb: null, max_storage_gb: null, revision: "" },
    host: {
      tasks: [],
      settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" },
      offsite: { configured: false, keyKept: false },
      connection: { mode: "local", panelHost: `console.${ROOT_DOMAIN}`, adminHost: `op.console.${ROOT_DOMAIN}` },
      storage: { mode: "local" },
    },
    authSources: {
      sources: [{ tag: "littleskin", prefix: "LS", url: "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined", api_url: "", enabled: true }],
      retired: [],
      revision: "installation-defaults",
      managed: false,
    },
    files: {},
    fileOps: {},
    fileExports: {},
  };
  for (const server of state.servers) {
    server.disk = { usedBytes: 64 * 2 ** 20, limitBytes: memoryLimit(server.storageSize || "8Gi")!, sampledAt: new Date().toISOString(), stale: false };
  }
  return state;
}

// generatedServers pads the fleet past one page (page size 20) with deterministic data.
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
    resumeAfterHostRestart: true,
    restartOnCrash: true,
    maxCrashRestarts: 3,
    notify: { started: false, stopped: false, problems: true },
    owner,
    ...overrides,
  };
}

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

// mockScan returns the stored scan of a seeded build.
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

// signInProven opens a session through a proven factor (email code, passkey, operator login), which counts as a re-auth.
function signInProven(ctx: RequestContext, accountID: AccountID): void {
  setSessionCookie(ctx.res, accountID);
  const acc = ctx.state.accounts[accountID];
  if (acc) acc.reauthUntil = Date.now() + REAUTH_MS;
}

const REAUTH_MS = 5 * 60_000;

// reauthFactors mirrors the backend reauthState; empty when there is nothing to re-prove.
function reauthFactors(state: MockState, acc: MockAccount): string[] {
  const hasPasskey = (state.passkeys[acc.id] ?? []).length > 0;
  if (!hasPasskey && !acc.emailVerified) return [];
  const factors = hasPasskey ? ["passkey"] : [];
  if (acc.role !== "user") factors.push("sign_in");
  else if (acc.emailVerified) factors.push("email");
  return factors;
}

// refusedForReauth answers 403 reauth_required for a sign-in change without a recent proof.
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

// thisSessionHash is the per-account session the mock cookie stands for.
function thisSessionHash(acc: MockAccount): string {
  return `mock-this-${acc.id}`;
}

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

function mockUsage(server: MockServer) {
  if (server.phase !== "Running" || server.desiredState !== "Running") return { current: null, history: [] };
  const now = Date.now();
  const history = Array.from({ length: 240 }, (_, i) => ({ at: new Date(now - (239 - i) * 15_000).toISOString(), cpuMilli: 250, memoryBytes: 512 * 2 ** 20 }));
  return { current: history.at(-1)!, history };
}

function mockCurrentUsage(server: MockServer) {
  const { current } = mockUsage(server);
  return current ? { cpuMilli: current.cpuMilli, memoryBytes: current.memoryBytes } : {};
}

function mockDiskUsage(server: MockServer) {
  const limitBytes = memoryLimit(server.storageSize || "8Gi");
  if (!server.disk || !limitBytes || !Number.isFinite(limitBytes)) return { disk: undefined, diskFull: server.diskFull };
  const disk = { ...server.disk, limitBytes: Math.ceil(limitBytes) };
  const age = Date.now() - Date.parse(disk.sampledAt);
  disk.stale = disk.stale || !Number.isFinite(age) || age < 0 || age > 180_000;
  if (Number.isFinite(age) && age >= 0 && disk.usedBytes >= disk.limitBytes) server.diskFull = true;
  else if (!disk.stale) server.diskFull = undefined;
  if (server.diskFull && server.desiredState === "Running") {
    const at = new Date().toISOString();
    server.autoStop = { reason: "DiskFull", at };
    const events = server.events ?? [];
    server.events = [...events, { seq: (events.at(-1)?.seq ?? 0) + 1, type: "DiskFull" as const, at, stopping: true }].slice(-20);
    setPhase(server, "Stopped");
  }
  return { disk, diskFull: server.diskFull };
}

// fleetView mirrors the Go fleetServerView (GET /fleet).
function fleetView(state: MockState, accountInfo: MockAccount): FleetServer[] {
  return state.servers.map((s, i) => {
    const disk = mockDiskUsage(s);
    const { owner, ...wire } = s;
    return {
      ...wire,
      ...disk,
      ...mockCurrentUsage(s),
      endpointMode: "domain",
      endpointAddress: s.ready ? `10.43.0.${10 + i}:25565` : undefined,
      playersOnline: s.ready ? s.playersOnline : 0,
      owner: owner ? state.accounts[owner].email : "",
      owned: owner === accountInfo.id,
      claimable: owner === null && !s.retiring && !s.reaperExempt,
      system: s.reaperExempt,
    };
  });
}

function myServerView(serverInfo: MockServer, accountInfo: MockAccount): MyServerView {
  const owned = canManage(accountInfo, serverInfo);
  const disk = mockDiskUsage(serverInfo);
  return {
    name: serverInfo.name,
    subdomain: serverInfo.subdomain,
    owned,
    claimable: serverInfo.owner === null && !serverInfo.retiring && accountInfo.linked && !owned,
    phase: serverInfo.phase,
    playersOnline: serverInfo.playersOnline,
    playersMax: serverInfo.playersMax,
    displayName: serverInfo.displayName,
    diskFull: disk.diskFull,
    ...(owned && {
      extraPorts: serverInfo.extraPorts,
      extraPortsPending: serverInfo.extraPortsPending,
      disk: disk.disk,
      cpu: serverInfo.cpu,
      memory: serverInfo.memory,
      ...mockCurrentUsage(serverInfo),
      desiredState: serverInfo.desiredState,
      autostartPolicy: serverInfo.autostartPolicy,
      playerCountUnknown: serverInfo.playerCountUnknown,
      retiring: serverInfo.retiring,
    }),
  };
}

// statusView returns the full projection to an owner or admin, the public subset to anyone else.
function statusView(serverInfo: MockServer, accountInfo: MockAccount): ServerStatus {
  const disk = mockDiskUsage(serverInfo);
  const { owner: _owner, ...wire } = serverInfo;
  if (canManage(accountInfo, serverInfo)) return { ...wire, ...disk, ...mockCurrentUsage(serverInfo) };
  return {
    name: wire.name,
    subdomain: wire.subdomain,
    displayName: wire.displayName,
    phase: wire.phase,
    ready: wire.ready,
    diskFull: wire.diskFull,
    playersOnline: wire.playersOnline,
    playersMax: wire.playersMax,
    idleStopSeconds: 0,
    resumeAfterHostRestart: false,
    restartOnCrash: false,
    maxCrashRestarts: 0,
    notify: { started: false, stopped: false, problems: false },
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
  if (!name || !subdomain || !req.image === !req.software) return "bad_request";
  if (findServer(state, name)) return "already_exists";
  if (state.servers.some((s) => s.subdomain === subdomain)) return "subdomain_taken";
  let image = req.image;
  let software: ServerSoftware | undefined;
  if (req.software) {
    if (!req.acceptEula) return "eula_required";
    const resolved = resolveSoftware(req.software);
    if (typeof resolved === "string") return resolved;
    software = resolved;
    image = runtimeImage(resolved.java);
  } else if (!state.images.some((i) => i.enabled && i.image_ref === req.image)) {
    return "image_not_whitelisted";
  }

  const created = server(name, req.displayName?.trim() || name, "Stopped", owner, {
    subdomain,
    playersOnline: 0,
    playersMax: 20,
    autostartPolicy: policy(req.autostartPolicy),
    image,
    software,
    memory: req.memory,
    storageSize: req.storage,
  });
  state.servers.unshift(created);
  state.files[name] = emptyVolume();
  return created;
}

function sendCreateError(res: ServerResponse, code: CreateError): void {
  const status = code === "already_exists" || code === "subdomain_taken" ? 409 : 400;
  sendError(res, status, code, code);
}

// MOCK_SOFTWARE is a catalog slice: versions newest first, each with its required Java, forwarding support and builds.
type MockSoftwareVersion = { id: string; stable: boolean; java: number; forwarding: boolean; builds?: SoftwareBuild[] };
const FABRIC_LOADERS: SoftwareBuild[] = [
  { id: "0.19.5", stable: true, recommended: true },
  { id: "0.19.4", stable: true },
];
const QUILT_LOADERS: SoftwareBuild[] = [
  { id: "0.31.0-beta.4", stable: false },
  { id: "0.30.1", stable: true, recommended: true },
];
const MOCK_SOFTWARE: Record<Exclude<SoftwareType, "custom">, MockSoftwareVersion[]> = {
  paper: [
    { id: "1.21.4", stable: true, java: 21, forwarding: true, builds: [
      { id: "232", stable: true, recommended: true }, { id: "231", stable: true }, { id: "230", stable: true }] },
    { id: "1.20.1", stable: true, java: 17, forwarding: true, builds: [{ id: "196", stable: true, recommended: true }] },
    { id: "1.16.5", stable: true, java: 8, forwarding: true, builds: [{ id: "794", stable: true, recommended: true }] },
    { id: "1.12.2", stable: true, java: 8, forwarding: false, builds: [{ id: "1620", stable: true, recommended: true }] },
  ],
  purpur: [
    { id: "1.21.4", stable: true, java: 21, forwarding: true, builds: [
      { id: "2416", stable: true, recommended: true }, { id: "2415", stable: true }] },
    { id: "1.20.1", stable: true, java: 17, forwarding: true, builds: [{ id: "2062", stable: true, recommended: true }] },
  ],
  fabric: [
    { id: "26.3", stable: true, java: 25, forwarding: true, builds: FABRIC_LOADERS },
    { id: "1.21.4", stable: true, java: 21, forwarding: true, builds: FABRIC_LOADERS },
    { id: "1.20.1", stable: true, java: 17, forwarding: true, builds: FABRIC_LOADERS },
  ],
  quilt: [
    { id: "1.21.4", stable: true, java: 21, forwarding: false, builds: QUILT_LOADERS },
    { id: "1.20.1", stable: true, java: 17, forwarding: true, builds: QUILT_LOADERS },
  ],
  forge: [
    { id: "26.3", stable: true, java: 25, forwarding: false, builds: [{ id: "66.0.12", stable: true, recommended: true }] },
    { id: "1.20.1", stable: true, java: 17, forwarding: true, builds: [
      { id: "47.4.10", stable: true, recommended: true }, { id: "47.4.9", stable: true }] },
    { id: "1.15.2", stable: true, java: 8, forwarding: true, builds: [
      { id: "31.2.62", stable: true }, { id: "31.2.57", stable: true, recommended: true }] },
    { id: "1.12.2", stable: true, java: 8, forwarding: false, builds: [
      { id: "14.23.5.2860", stable: true }, { id: "14.23.5.2859", stable: true, recommended: true }] },
  ],
  neoforge: [
    { id: "26.3", stable: true, java: 25, forwarding: false, builds: [{ id: "26.3.0.85", stable: true, recommended: true }] },
    { id: "1.21.4", stable: true, java: 21, forwarding: true, builds: [{ id: "21.4.150", stable: true, recommended: true }] },
  ],
  vanilla: [
    { id: "26.4-snapshot-4", stable: false, java: 25, forwarding: false },
    { id: "26.3", stable: true, java: 25, forwarding: false },
    { id: "1.21.4", stable: true, java: 21, forwarding: false },
    { id: "1.20.1", stable: true, java: 17, forwarding: false },
    { id: "1.16.5", stable: true, java: 8, forwarding: false },
    { id: "1.12.2", stable: true, java: 8, forwarding: false },
    { id: "1.7.10", stable: true, java: 8, forwarding: false },
  ],
};
const SOFTWARE_TYPES: SoftwareType[] = ["paper", "purpur", "fabric", "quilt", "forge", "neoforge", "vanilla", "custom"];
const RUNTIMES = [8, 17, 21, 25] as const;
type Runtime = (typeof RUNTIMES)[number];
type SoftwareError = "bad_software" | "software_not_found" | "java_too_old" | "java_unavailable";

function softwareVersion(type: string, mc: string | undefined): MockSoftwareVersion | undefined {
  return Object.hasOwn(MOCK_SOFTWARE, type) ? MOCK_SOFTWARE[type as Exclude<SoftwareType, "custom">].find((v) => v.id === mc) : undefined;
}

// jarPathOK mirrors software.ValidateJarPath.
function jarPathOK(p: string | undefined): boolean {
  if (!p || p.startsWith("/") || !p.endsWith(".jar")) return false;
  const parts = p.split("/");
  return parts.every((s) => s !== "" && s !== "." && s !== "..") && parts[0] !== ".felis";
}

// resolveSoftware defaults to the recommended build and the earliest runtime that fits the version.
function resolveSoftware(req: SoftwareRequest): ServerSoftware | SoftwareError {
  if (!SOFTWARE_TYPES.includes(req.type)) return "bad_software";
  if (req.java !== undefined && !RUNTIMES.includes(req.java)) return "bad_software";
  if (req.type === "custom") {
    if (!jarPathOK(req.jar)) return "bad_software";
    return { type: "custom", jar: req.jar, java: req.java ?? 21, forwarding: "none" };
  }
  const v = softwareVersion(req.type, req.minecraftVersion);
  if (!v) return "software_not_found";
  let build: string | undefined;
  if (v.builds) {
    build = req.build || (v.builds.find((b) => b.recommended) ?? v.builds[0]).id;
    if (!v.builds.some((b) => b.id === build)) return "software_not_found";
  }
  const runtime = RUNTIMES.find((r) => r >= v.java);
  if (!runtime) return "java_unavailable";
  if (req.java !== undefined && req.java < v.java) return "java_too_old";
  return { type: req.type, minecraftVersion: v.id, build, java: (req.java ?? runtime) as Runtime, forwarding: v.forwarding ? "velocity" : "none" };
}

// runtimeImage returns the Temurin runtime reference pinned by digest.
function runtimeImage(java: number): string {
  const ref = `registry.felis.svc:5000/mirror/eclipse-temurin:${java}-jre`;
  return `${ref}@sha256:${createHash("sha256").update(ref).digest("hex")}`;
}

function quantityMB(q: string | undefined): number {
  const m = /^(\d+(?:\.\d+)?)(Gi|Mi)$/.exec(q ?? "");
  if (!m) return 0;
  return Math.round(Number(m[1]) * (m[2] === "Gi" ? 1024 : 1));
}

function cpuMilli(q: string | undefined): number {
  if (!q) return 0;
  return q.endsWith("m") ? Number(q.slice(0, -1)) || 0 : Math.round((Number(q) || 0) * 1000);
}

// quotaState picks the user's own quota, else the default when enabled, else none.
function quotaState(state: MockState, acc: MockAccount): Omit<ServerQuota, "canCreate" | "reason"> {
  const owned = state.servers.filter((s) => s.owner === acc.id);
  const usage = {
    servers: owned.length,
    cpu_milli: owned.reduce((n, s) => n + cpuMilli(s.cpu), 0),
    memory_mb: owned.reduce((n, s) => n + quantityMB(s.memory), 0),
    storage_mb: owned.reduce((n, s) => n + quantityMB(s.storageSize), 0),
  };
  const caps = (q: { max_servers?: number | null; max_cpu_milli?: number | null; max_memory_mb?: number | null; max_storage_gb?: number | null }) => ({
    ...(q.max_servers != null && { max_servers: q.max_servers }),
    ...(q.max_cpu_milli != null && { max_cpu_milli: q.max_cpu_milli }),
    ...(q.max_memory_mb != null && { max_memory_mb: q.max_memory_mb }),
    ...(q.max_storage_gb != null && { max_storage_gb: q.max_storage_gb }),
  });
  if (acc.quota) return { source: "user", limits: caps(acc.quota), usage };
  if (state.selfService.enabled) return { source: "default", limits: caps(state.selfService), usage };
  return { source: "", limits: null, usage };
}

function fitsQuota(q: Omit<ServerQuota, "canCreate" | "reason">, add: { servers: number; cpu_milli: number; memory_mb: number; storage_mb: number }): boolean {
  const l = q.limits ?? {};
  const u = q.usage;
  return (l.max_servers == null || u.servers + add.servers <= l.max_servers) &&
    (l.max_cpu_milli == null || u.cpu_milli + add.cpu_milli <= l.max_cpu_milli) &&
    (l.max_memory_mb == null || u.memory_mb + add.memory_mb <= l.max_memory_mb) &&
    (l.max_storage_gb == null || u.storage_mb + add.storage_mb <= l.max_storage_gb * 1024);
}

const STARTUP_KEYS = ["java_flags", "args", "env", "stop_commands"] as const;
const RESERVED_STARTUP_ENV = new Set([
  "JAVA_MEMORY", "JAVA_FLAGS", "JAVA_HOME", "JRE_HOME", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH",
  "RCON_PORT", "RCON_PASSWORD", "FABRIC_PROXY_SECRET", "FABRIC_PROXY_HACK_MESSAGE_CHAIN",
  "PATH", "HOME", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH", "GLOBIGNORE",
  "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "GCONV_PATH", "GLIBC_TUNABLES",
]);

function reservedStartupEnv(name: string): boolean {
  return name.startsWith("FELIS_") || RESERVED_STARTUP_ENV.has(name);
}

function mockStartupSettings(state: MockState, name: string): ServerStartupSettings {
  return state.startupSettings.get(name) ?? { java_flags: [], args: [], env: [], stop_commands: [] };
}

function trimStartupCommand(command: string): string {
  const trim = (value: string) => value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
  return trim(trim(command).replace(/^\//, ""));
}

function patchStartupSettings(current: ServerStartupSettings, patch: StartupSettingsPatch): ServerStartupSettings | "bad_request" | "bad_startup_settings" {
  const next = structuredClone(current);
  const text = (value: string, limit: number) => Buffer.byteLength(value) <= limit && !value.includes("\0");
  for (const key of ["java_flags", "args", "stop_commands"] as const) {
    const value = patch[key];
    if (value == null) continue;
    if (!Array.isArray(value) || value.some((item) => item !== null && typeof item !== "string")) return "bad_request";
    next[key] = value.map((item) => Buffer.from(item ?? "").toString("utf8"));
  }
  if (patch.env != null) {
    if (!Array.isArray(patch.env)) return "bad_request";
    const env: ServerStartupSettings["env"] = [];
    for (const item of patch.env) {
      if (item === null) return "bad_startup_settings";
      if (typeof item !== "object" || Array.isArray(item) || Object.keys(item).some((key) => key !== "name" && key !== "value")) return "bad_request";
      if ((item.name != null && typeof item.name !== "string") || (item.value != null && typeof item.value !== "string")) return "bad_request";
      const name = Buffer.from(item.name ?? "").toString("utf8");
      if (reservedStartupEnv(name)) return "bad_startup_settings";
      env.push({ name, value: Buffer.from(item.value ?? "").toString("utf8") });
    }
    next.env = [...current.env.filter((entry) => reservedStartupEnv(entry.name)), ...env];
  }
  if (patch.stop_commands != null) {
    if (next.stop_commands.some((command) => !text(command, 1024))) return "bad_startup_settings";
    next.stop_commands = next.stop_commands.map(trimStartupCommand);
  }
  const env = next.env.filter((entry) => !reservedStartupEnv(entry.name));
  if (next.java_flags.length > 128 || next.args.length > 128 || env.length > 64 || next.stop_commands.length > 16) return "bad_startup_settings";
  const heapPrefixes = ["-Xmx", "-Xms", "-mx", "-ms", "-XX:MaxHeapSize", "-XX:InitialHeapSize", "-XX:MinHeapSize", "-XX:VMOptionsFile", "-XX:Flags", "@"];
  if (next.java_flags.some((flag) => !text(flag, 1024) || !flag || /[\p{White_Space}\p{Cc}]/u.test(flag) || heapPrefixes.some((prefix) => flag.startsWith(prefix)))) return "bad_startup_settings";
  if (next.args.some((arg) => !text(arg, 2048))) return "bad_startup_settings";
  const names = new Set<string>();
  for (const entry of env) {
    if (Buffer.byteLength(entry.name) > 128 || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(entry.name) || names.has(entry.name) || !text(entry.value, 4096)) return "bad_startup_settings";
    names.add(entry.name);
  }
  if (next.stop_commands.some((raw) => !text(raw, 1024) || !trimStartupCommand(raw) || /[\u0000-\u001f]/.test(trimStartupCommand(raw)))) return "bad_startup_settings";
  const total = [...next.java_flags, ...next.args, ...next.stop_commands, ...env.flatMap((entry) => [entry.name, entry.value])]
    .reduce((sum, value) => sum + Buffer.byteLength(value), 0);
  return total <= 65536 ? next : "bad_startup_settings";
}

function prepareMockExtraPorts(state: MockState, server: MockServer, raw: unknown): ServerExtraPort[] | "bad_request" | "bad_extra_ports" {
  if (!Array.isArray(raw)) return "bad_request";
  if (raw.length > 5) return "bad_extra_ports";
  const keys = new Set<string>();
  const occupied = new Set([30443, ...state.servers.flatMap((entry) => entry.extraPorts?.flatMap((port) => port.nodePort ? [port.nodePort] : []) ?? [])]);
  const ports: ServerExtraPort[] = [];
  for (const item of raw) {
    if (item !== null && (typeof item !== "object" || Array.isArray(item))) return "bad_request";
    const fields = (item ?? {}) as Record<string, unknown>;
    if (Object.keys(fields).some((key) => key !== "protocol" && key !== "port")) return "bad_request";
    const protocol = fields.protocol ?? "";
    const port = fields.port ?? 0;
    if (typeof protocol !== "string" || typeof port !== "number" || !Number.isInteger(port) || port < -2147483648 || port > 2147483647) return "bad_request";
    if ((protocol !== "TCP" && protocol !== "UDP") || port < 1 || port > 65535 ||
      (protocol === "TCP" && (port === 25565 || port === 25575)) || keys.has(`${protocol}:${port}`)) return "bad_extra_ports";
    keys.add(`${protocol}:${port}`);
    let nodePort = server.extraPorts?.find((entry) => entry.protocol === protocol && entry.port === port)?.nodePort ?? ports.find((entry) => entry.port === port)?.nodePort;
    if (nodePort === undefined) {
      for (let candidate = 30000; candidate <= 32767; candidate++) {
        if (!occupied.has(candidate)) { nodePort = candidate; occupied.add(candidate); break; }
      }
    }
    ports.push({ protocol, port, ...(nodePort !== undefined && { nodePort, address: `203.0.113.10:${nodePort}` }) });
  }
  return ports;
}

async function patchMockServer(ctx: SessionContext, serverInfo: MockServer, ownerPatch: boolean): Promise<void> {
  const body = await readJSON<{
    displayName?: string; autostartPolicy?: AutostartPolicy; idleStopSeconds?: number; memory?: string;
    cpu?: string; image?: string; confirmImageChange?: boolean; resources?: { cpu?: string };
    software?: SoftwareRequest; confirmSoftwareChange?: boolean; acceptEula?: boolean;
    resumeAfterHostRestart?: boolean; restartOnCrash?: boolean; maxCrashRestarts?: number; notify?: Partial<ServerNotify>;
    extraPorts?: unknown;
  } & StartupSettingsPatch>(ctx.req).catch(() => null);
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    sendError(ctx.res, 400, "bad_request", "invalid request body");
    return;
  }
  const allowed = new Set<string>([
    ...STARTUP_KEYS, "extraPorts", "displayName", "autostartPolicy", "idleStopSeconds", "memory", "software",
    "confirmSoftwareChange", "acceptEula", "resumeAfterHostRestart", "restartOnCrash", "maxCrashRestarts", "notify",
    ...(ownerPatch ? ["cpu"] : ["image", "confirmImageChange", "resources", "storage"]),
  ]);
  if (Object.keys(body).some((key) => !allowed.has(key))) {
    sendError(ctx.res, 400, "bad_request", "unknown server patch field");
    return;
  }
  const changed: string[] = [];
  const next: Partial<MockServer> = {};
  if (body.extraPorts != null) {
    const ports = prepareMockExtraPorts(ctx.state, serverInfo, body.extraPorts);
    if (typeof ports === "string") {
      sendError(ctx.res, 400, ports, "invalid additional port settings");
      return;
    }
    next.extraPorts = ports;
    next.extraPortsPending = ports.some((port) => port.nodePort === undefined) || undefined;
    changed.push("extraPorts");
  }
  if (body.image !== undefined && body.software) {
    sendError(ctx.res, 400, "bad_request", "give either an image or server software, not both");
    return;
  }
  if (body.image !== undefined && !ownerPatch) {
    if (!ctx.state.images.some((i) => i.enabled && i.image_ref === body.image)) {
      sendError(ctx.res, 400, "image_not_whitelisted", "image is not whitelisted");
      return;
    }
    if (body.image !== serverInfo.image) {
      if (!body.confirmImageChange) {
        sendError(ctx.res, 409, "image_change_unconfirmed", "changing the image opens this world with the new image's Minecraft version; back the world up first, then resend with confirmImageChange");
        return;
      }
      Object.assign(next, { image: body.image, software: undefined });
      changed.push("image");
    }
  }
  if (body.software) {
    const cur = serverInfo.software;
    if (!cur && !body.acceptEula) {
      sendError(ctx.res, 400, "eula_required", "accept the Minecraft EULA (acceptEula) to run server software");
      return;
    }
    const sw = resolveSoftware(body.software);
    if (typeof sw === "string") {
      sendError(ctx.res, 400, sw, sw);
      return;
    }
    const moves = !cur || cur.type !== sw.type || cur.minecraftVersion !== sw.minecraftVersion;
    if (moves && !body.confirmSoftwareChange) {
      sendError(ctx.res, 409, "software_change_unconfirmed", `moving this server to ${sw.type} ${sw.minecraftVersion ?? ""} opens its world with that software and version; back the world up first, then resend with confirmSoftwareChange`);
      return;
    }
    Object.assign(next, { software: sw, image: runtimeImage(sw.java) });
    changed.push("software");
  }
  const cpu = ownerPatch ? body.cpu : body.resources?.cpu;
  if (body.memory !== undefined || cpu !== undefined) {
    if (ownerPatch && serverInfo.owner) {
      const q = quotaState(ctx.state, ctx.account);
      const add = {
        servers: 0, storage_mb: 0,
        memory_mb: body.memory !== undefined ? quantityMB(body.memory) - quantityMB(serverInfo.memory) : 0,
        cpu_milli: cpu !== undefined ? cpuMilli(cpu) - cpuMilli(serverInfo.cpu) : 0,
      };
      if (q.source && !fitsQuota(q, add)) {
        sendError(ctx.res, 403, "quota_exceeded", "this change does not fit in your quota");
        return;
      }
    }
    if (body.memory !== undefined) {
      next.memory = body.memory;
      next.javaMemory = `${Math.floor(quantityMB(body.memory) * 0.75)}M`;
      changed.push("memory");
    }
    if (cpu !== undefined) {
      next.cpu = cpu || undefined;
      changed.push("resources");
    }
  }
  if (body.displayName !== undefined) {
    next.displayName = body.displayName.trim() || serverInfo.name;
    changed.push("displayName");
  }
  if (body.autostartPolicy !== undefined) {
    next.autostartPolicy = policy(body.autostartPolicy);
    changed.push("autostartPolicy");
  }
  if (body.idleStopSeconds !== undefined) {
    if (body.idleStopSeconds !== 0 && (body.idleStopSeconds < 60 || body.idleStopSeconds > 86400)) {
      sendError(ctx.res, 400, "bad_request", "idleStopSeconds must be 0 or 60-86400");
      return;
    }
    next.idleStopSeconds = body.idleStopSeconds;
    changed.push("idleStopSeconds");
  }
  const recovery = body.resumeAfterHostRestart !== undefined || body.restartOnCrash !== undefined || body.maxCrashRestarts !== undefined || body.notify !== undefined;
  if (recovery && serverInfo.reaperExempt) {
    sendError(ctx.res, 400, "bad_request", "system services always restart and resume, and their problems go to the platform watchdog");
    return;
  }
  if (body.maxCrashRestarts !== undefined) {
    if (!Number.isInteger(body.maxCrashRestarts) || body.maxCrashRestarts < 1 || body.maxCrashRestarts > 20) {
      sendError(ctx.res, 400, "bad_crash_restarts", "maxCrashRestarts must be between 1 and 20");
      return;
    }
    next.maxCrashRestarts = body.maxCrashRestarts;
    changed.push("maxCrashRestarts");
  }
  if (body.resumeAfterHostRestart !== undefined) {
    next.resumeAfterHostRestart = body.resumeAfterHostRestart;
    changed.push("resumeAfterHostRestart");
  }
  if (body.restartOnCrash !== undefined) {
    next.restartOnCrash = body.restartOnCrash;
    changed.push("restartOnCrash");
  }
  if (body.notify) {
    next.notify = { ...serverInfo.notify, ...body.notify };
    changed.push("notify");
  }
  const startupKeys = STARTUP_KEYS.filter((key) => body[key] != null);
  let startup: ServerStartupSettings | undefined;
  if (startupKeys.length) {
    const prepared = patchStartupSettings(mockStartupSettings(ctx.state, serverInfo.name), body);
    if (typeof prepared === "string") {
      sendError(ctx.res, 400, prepared, "invalid startup settings");
      return;
    }
    startup = prepared;
    changed.push(...startupKeys);
  } else if (!changed.length && Object.keys(body).every((key) => key === "extraPorts" || STARTUP_KEYS.some((setting) => setting === key))) {
    sendError(ctx.res, 400, "bad_request", "no server fields were supplied");
    return;
  }
  Object.assign(serverInfo, next);
  if (startup) ctx.state.startupSettings.set(serverInfo.name, startup);
  sendJSON(ctx.res, 200, { name: serverInfo.name, patched: changed });
}

// handleMockSoftware serves the software catalog, the caller's quota, self-service server creation and the default quota.
async function handleMockSoftware(ctx: SessionContext): Promise<boolean> {
  const key = route(ctx);
  const [, type, , version] = ctx.parts.slice(2).map(decodeURIComponent);
  if (key === "GET software") {
    sendJSON(ctx.res, 200, { types: SOFTWARE_TYPES.map((t) => ({ type: t, builds: t !== "vanilla" && t !== "custom" })), runtimes: RUNTIMES });
    return true;
  }
  if (/^GET software\/[^/]+\/versions(\/[^/]+)?$/.test(key)) {
    const versions = Object.hasOwn(MOCK_SOFTWARE, type ?? "") ? MOCK_SOFTWARE[type as Exclude<SoftwareType, "custom">] : undefined;
    if (!versions) {
      sendError(ctx.res, 404, "not_found", `no versions for "${type}"`);
    } else if (version === undefined) {
      sendJSON(ctx.res, 200, { versions: versions.map(({ id, stable }) => ({ id, stable })) });
    } else {
      const v = versions.find((x) => x.id === version);
      if (!v) sendError(ctx.res, 404, "software_not_found", `${type} does not offer Minecraft "${version}"`);
      else sendJSON(ctx.res, 200, { version: v.id, requiredJava: v.java, java: RUNTIMES.find((r) => r >= v.java), builds: v.builds ?? [], forwarding: v.forwarding ? "velocity" : "none" });
    }
    return true;
  }
  if (key === "GET me/server-quota") {
    const q = quotaState(ctx.state, ctx.account);
    const full = q.limits?.max_servers != null && q.usage.servers >= q.limits.max_servers;
    const reason = !ctx.account.linked ? "not_linked" : !q.source ? "self_service_disabled" : full ? "quota_full" : undefined;
    sendJSON(ctx.res, 200, { ...q, canCreate: !reason, ...(reason && { reason }) });
    return true;
  }
  if (key === "POST me/servers") {
    const body = await readJSON<Partial<SelfCreateRequest>>(ctx.req);
    const name = body.name?.trim() ?? "";
    const subdomain = body.subdomain?.trim() ?? "";
    if (!ctx.account.linked) {
      sendError(ctx.res, 412, "not_linked", "link your Minecraft account before creating a server");
    } else if (!/^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(name)) {
      sendError(ctx.res, 400, "bad_name", "invalid server name");
    } else if (!/^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(subdomain)) {
      sendError(ctx.res, 400, "bad_subdomain", "invalid subdomain");
    } else if (!body.acceptEula) {
      sendError(ctx.res, 400, "eula_required", "accept the Minecraft EULA (acceptEula) to run server software");
    } else {
      const sw = body.software ? resolveSoftware(body.software) : "bad_software";
      const memory = quantityMB(body.memory);
      const storage = quantityMB(body.storage);
      const q = quotaState(ctx.state, ctx.account);
      if (typeof sw === "string") sendError(ctx.res, 400, sw, sw);
      else if (!memory || !storage) sendError(ctx.res, 400, "bad_request", "memory and storage are required, e.g. 2Gi and 10Gi");
      else if (!q.source) sendError(ctx.res, 403, "self_service_disabled", "creating servers is not open to your account");
      else if (!fitsQuota(q, { servers: 1, cpu_milli: cpuMilli(body.cpu), memory_mb: memory, storage_mb: storage })) sendError(ctx.res, 403, "quota_exceeded", "this server does not fit in your quota");
      else if (ctx.state.servers.some((s) => s.subdomain === subdomain)) sendError(ctx.res, 409, "subdomain_taken", `subdomain "${subdomain}" is already in use`);
      else if (findServer(ctx.state, name)) sendError(ctx.res, 409, "already_exists", `a server named "${name}" already exists`);
      else {
        const created = server(name, body.displayName?.trim() || name, "Stopped", ctx.account.id, {
          subdomain, playersOnline: 0, playersMax: 20, autostartPolicy: policy(body.autostartPolicy),
          image: runtimeImage(sw.java), software: sw, memory: body.memory, cpu: body.cpu, storageSize: body.storage,
          javaMemory: `${Math.floor(memory * 0.75)}M`,
        });
        ctx.state.servers.unshift(created);
        ctx.state.files[name] = emptyVolume();
        sendJSON(ctx.res, 201, { name, subdomain, desiredState: created.desiredState });
      }
    }
    return true;
  }
  const own = key.match(/^PATCH me\/servers\/([^/]+)$/);
  if (own) {
    const serverInfo = findServer(ctx.state, decodeURIComponent(own[1]));
    if (!serverInfo) sendError(ctx.res, 404, "not_found", "server not found");
    else if (!serverInfo.owner || serverInfo.owner !== ctx.account.id) sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    else await patchMockServer(ctx, serverInfo, true);
    return true;
  }
  if (key === "GET settings/self-service" || key === "PUT settings/self-service") {
    if (!isOwner(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "Owner account required");
    } else if (is("GET", ctx)) {
      sendJSON(ctx.res, 200, ctx.state.selfService);
    } else {
      const body = await readJSON<SelfServiceQuotaSettings>(ctx.req);
      const cap = (v: unknown) => v === null || (Number.isInteger(v) && (v as number) >= 0 && (v as number) <= 2147483647);
      if (body.revision !== ctx.state.selfService.revision) {
        sendError(ctx.res, 409, "conflict", "the default quota changed; reload before saving");
      } else if (typeof body.enabled !== "boolean" || ![body.max_servers, body.max_cpu_milli, body.max_memory_mb, body.max_storage_gb].every(cap)) {
        sendError(ctx.res, 400, "invalid_quota", "each cap must be a whole number from 0 to 2147483647, or null for unlimited");
      } else {
        const { revision: _revision, ...quota } = body;
        ctx.state.selfService = { ...quota, revision: createHash("sha256").update(JSON.stringify(quota)).digest("hex") };
        sendJSON(ctx.res, 200, ctx.state.selfService);
      }
    }
    return true;
  }
  return false;
}

// Op-login requests auto-approve after a few seconds; an "expire…" address lapses instead.
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
    // The mock install always has an Owner.
    case "GET auth/owner-status":
      sendJSON(ctx.res, 200, { owner_bound: true });
      return true;
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

// hostArchivePolicy returns the felis.toml default retention (reaper.DefaultConfig).
function hostArchivePolicy(): ArchivePolicySettings {
  return { manualKeep: 5, manualRetentionDays: 30, manualCooldownMinutes: 10, scheduledEveryHours: 24, scheduledKeep: 7, scheduledRetentionDays: 90, releasedRetentionDays: 90, maxStoreGiB: 0, auditRetentionDays: 365, source: "host", revision: "" };
}

function mockHostOverview(state: MockState): HostOverview {
  const day = (n: number) => new Date(Date.now() - n * 86_400_000);
  const stamp = (d: Date) => d.toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
  const h = state.host;
  return {
    host: "felis-mock", at: new Date().toISOString(), version: "v0.4.0-dev",
    disks: [{ path: "/var/lib", total: 120 * 2 ** 30, available: 71 * 2 ** 30 }],
    memory: { total: 16 * 2 ** 30, available: 9 * 2 ** 30 },
    units: [
      { name: "k3s.service", active: "active" },
      { name: "felis-velocity.service", active: "active" },
      { name: "felis-node-control.service", active: "active" },
      { name: "felis-db-backup.timer", active: "active", next: new Date(Date.now() + 6 * 3600_000).toISOString(), last: day(0.4).toISOString() },
      { name: "felis-watchdog.timer", active: "active", next: new Date(Date.now() + 90_000).toISOString() },
      ...(h.offsite.configured ? [{ name: "felis-offsite.timer", active: "active", next: new Date(Date.now() + 1800_000).toISOString() }] : []),
    ],
    proxy: { address: "127.0.0.1:25565", reachable: true },
    alerts: [],
    ...(h.quietUntil && new Date(h.quietUntil) > new Date() ? { quietUntil: h.quietUntil } : {}),
    databaseBackups: [0.4, 1.4, 2.4].map((n, i) => ({ name: `felis-db-${stamp(day(n))}-${i === 1 ? "manual" : "daily"}.tar`, label: i === 1 ? "manual" : "daily", size: 3_482_112 - i * 4096, created: day(n).toISOString() })),
    offsite: h.offsite,
    settings: h.settings,
    connection: h.connection,
    storage: h.storage,
    rootDomain: ROOT_DOMAIN,
  };
}

const mockStages: Partial<Record<NodeControlRequest["action"], string[]>> = {
  "db-backup": ["database_backup"], "db-verify": ["verify"], "db-restore": ["verify", "stop_control_plane", "restore_database", "migrate", "start_control_plane"],
  "offsite-configure": ["configure", "install_timer", "check_bucket"], "backup-now": ["backup"], "rotate-token": ["rotate"], "domain-set": ["domain"],
  "connection-configure": ["configure"], "storage-configure": ["configure"], "host-settings": ["configure"], "restore-host": ["rebuild"],
};

function runMockHostTask(state: MockState, request: NodeControlRequest, actor: string): NodeControlTask {
  const id = createHash("sha256").update(`${Date.now()}-${Math.random()}`).digest("hex").slice(0, 32);
  const now = new Date().toISOString();
  const p = request.params ?? {};
  const h = state.host;
  const stages = ["preflight", ...(mockStages[request.action] ?? []), "complete"];
  const task: NodeControlTask = {
    id, request: { ...request, secrets: undefined }, actor, state: "succeeded", stage: "complete", startedAt: now, finishedAt: now,
    log: stages.map((s) => `[felis] ${s}`).join("\n") + `\nfelis ${request.action}: done (mock)\n`,
  };
  switch (request.action) {
    case "doctor":
      task.result = {
        host: "felis-mock", at: now, source: "checks run as /etc/systemd/system/felis-watchdog.service runs them (config /etc/felis/felis.host.toml)", critical: 0, warning: 1,
        areas: [
          { key: "config", title: "configuration", state: "ok" }, { key: "cluster", title: "Kubernetes cluster", state: "ok" }, { key: "postgres", title: "PostgreSQL", state: "ok" },
          { key: "proxy", title: "game proxy", state: "ok" }, { key: "db-backup", title: "database backups", state: "ok" },
          { key: "offsite", title: "off-site copy", ...(h.offsite.configured ? { state: "ok" } : { state: "skipped", skipped: "not configured" }) },
          { key: "disk", title: "disk space", state: "ok" }, { key: "memory", title: "memory", state: "ok" },
          { key: "alerts", title: "alerting", state: "problem", findings: [{ key: "watchdog/heartbeat", severity: "warning", summary: "未设置外部心跳检查", summary_en: "no heartbeat URL is set: a host that goes down entirely alerts no one", hint: "set one in the panel (Platform settings → Monitoring heartbeat)" }] },
        ],
      };
      break;
    case "support-bundle":
      task.artifact = `/var/lib/felis/support/felis-support-${now.replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z")}.tar.gz`;
      break;
    case "host-settings":
      h.settings = { ...h.settings, ...(p.dbBackupTime ? { dbBackupTime: p.dbBackupTime } : {}), ...(p.dbBackupKeep ? { dbBackupKeep: Number(p.dbBackupKeep) } : {}), ...(p.velocityXmx ? { velocityXmx: p.velocityXmx } : {}) };
      break;
    case "offsite-configure":
      h.offsite = { configured: true, endpoint: p.endpoint, bucket: p.bucket, region: p.region, prefix: p.prefix, dbKeep: p.dbKeep ? Number(p.dbKeep) : 30, keyKept: true, status: { last_attempt: now, last_success: now, bucket: p.bucket, key_id: "mock-key" } };
      break;
    case "offsite-disable":
      h.offsite = { configured: false, keyKept: h.offsite.keyKept };
      break;
    case "connection-configure":
      h.connection = { mode: p.mode as HostOverview["connection"]["mode"], panelHost: p.panelHost || h.connection.panelHost, adminHost: p.adminHost || h.connection.adminHost };
      break;
    case "storage-configure":
      h.storage = p.mode === "s3" ? { mode: "s3", endpoint: p.endpoint, bucket: p.bucket, region: p.region } : { mode: "local" };
      break;
  }
  h.tasks = [task, ...h.tasks].slice(0, 100);
  return task;
}

// handleMockHost serves the host-service routes; owner only.
async function handleMockHost(ctx: SessionContext): Promise<boolean> {
  const key = route(ctx);
  const taskRoute = key.match(/^(GET|POST) settings\/node-control\/tasks\/([a-f0-9]+)(\/retry|\/artifact)?$/);
  const owned = ["GET settings/node-control", "POST settings/node-control/tasks", "GET platform/host", "POST platform/host/quiet", "POST platform/offsite/key", "GET platform/audit-export"].includes(key);
  if (!taskRoute && !owned) return false;
  if (!isOwner(ctx.account.role)) {
    sendError(ctx.res, 403, "forbidden", "Owner account required");
    return true;
  }
  const h = ctx.state.host;
  const strip = (t: NodeControlTask) => ({ ...t, log: undefined });
  if (taskRoute) {
    const task = h.tasks.find((t) => t.id === taskRoute[2]);
    if (!task) sendError(ctx.res, 404, "not_found", "node task not found");
    else if (taskRoute[3] === "/retry") sendJSON(ctx.res, 202, runMockHostTask(ctx.state, task.request, ctx.account.id));
    else if (taskRoute[3] === "/artifact") {
      ctx.res.statusCode = 200;
      ctx.res.setHeader("Content-Type", "application/gzip");
      ctx.res.setHeader("Content-Disposition", `attachment; filename="${task.artifact?.split("/").pop() ?? "felis-support.tar.gz"}"`);
      ctx.res.end(gzipSync(Buffer.from("felis support bundle (mock)\n")));
    } else sendJSON(ctx.res, 200, task);
    return true;
  }
  switch (key) {
    case "GET settings/node-control":
      sendJSON(ctx.res, 200, { available: true, tasks: h.tasks.map(strip) });
      return true;
    case "POST settings/node-control/tasks": {
      const body = await readJSON<NodeControlRequest>(ctx.req);
      if (h.tasks.some((t) => t.state === "running")) sendError(ctx.res, 409, "node_operation_busy", "a host task is already running");
      else sendJSON(ctx.res, 202, strip(runMockHostTask(ctx.state, body, ctx.account.id)));
      return true;
    }
    case "GET platform/host":
      sendJSON(ctx.res, 200, mockHostOverview(ctx.state));
      return true;
    case "POST platform/host/quiet": {
      const body = await readJSON<{ until: string | null }>(ctx.req);
      h.quietUntil = body.until ?? undefined;
      ctx.res.statusCode = 204;
      ctx.res.end();
      return true;
    }
    case "POST platform/offsite/key":
      if (!h.offsite.keyKept) sendError(ctx.res, 404, "not_found", "no off-site key is kept on this host");
      else sendJSON(ctx.res, 200, { key: "bW9jay1vZmZzaXRlLWtleS1rZXB0LW9uLXRoZS1ob3N0IQ==" });
      return true;
    case "GET platform/audit-export":
      ctx.res.statusCode = 200;
      ctx.res.setHeader("Content-Type", "application/x-ndjson");
      ctx.res.setHeader("Content-Disposition", `attachment; filename="felis-audit.jsonl"`);
      ctx.res.end(`{"action":"server.create","actor":"owner@mock.felis.local"}\n`);
      return true;
  }
  return false;
}

async function handleSession(ctx: SessionContext): Promise<boolean> {
  if (ctx.parts[2] === "exports" && ctx.parts[3] && ctx.method === "GET") {
    const item = ctx.state.fileExports[ctx.parts[3]];
    if (!item || item.owner !== ctx.account.id) sendError(ctx.res, 404, "not_found", "no such export");
    else if (item.spent || item.expires < Date.now()) sendError(ctx.res, 410, "export_expired", "ticket expired");
    else if (ctx.parts[4] === "download") {
      item.spent = true;
      ctx.res.setHeader("Content-Type", item.contentType);
      ctx.res.setHeader("Content-Length", item.data.length);
      ctx.res.setHeader("Content-Disposition", `attachment; filename="${item.filename.replaceAll('"', "")}"`);
      ctx.res.end(item.data);
    } else sendJSON(ctx.res, 200, { state: "ready" });
    return true;
  }
  if (await handleMockHost(ctx)) return true;
  if (await handleMockSoftware(ctx)) return true;
  switch (route(ctx)) {
    case "GET settings/entry-policy":
    case "PUT settings/entry-policy": {
      if (!isOwner(ctx.account.role)) sendError(ctx.res, 403, "forbidden", "Owner account required");
      else if (is("GET", ctx)) sendJSON(ctx.res, 200, ctx.state.entryPolicy);
      else {
        const body = await readJSON<EntryPolicySettings>(ctx.req);
        if (body.revision !== ctx.state.entryPolicy.revision) sendError(ctx.res, 409, "conflict", "Entry policy changed");
        else {
          ctx.state.entryPolicy = { ...body, revision: createHash("sha256").update(JSON.stringify(body)).digest("hex") };
          sendJSON(ctx.res, 200, ctx.state.entryPolicy);
        }
      }
      return true;
    }
    case "GET settings/wake-policy":
    case "PUT settings/wake-policy": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
      } else if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.wakePolicy);
      } else {
        const body = await readJSON<Omit<WakePolicySettings, "managed">>(ctx.req);
        if (body.revision !== ctx.state.wakePolicy.revision) sendError(ctx.res, 409, "conflict", "policy changed; reload before saving");
        else if (!Number.isInteger(body.maxRunningServers) || body.maxRunningServers < 0 || body.maxRunningServers > 10000 || !Number.isInteger(body.wakeCooldownSeconds) || body.wakeCooldownSeconds < 0 || body.wakeCooldownSeconds > 3600) sendError(ctx.res, 400, "bad_request", "invalid wake policy");
        else {
          ctx.state.wakePolicy = { ...body, revision: createHash("sha256").update(JSON.stringify(body)).digest("hex"), managed: true };
          sendJSON(ctx.res, 200, ctx.state.wakePolicy);
        }
      }
      return true;
    }
    // A host starting with "smtp.fail" refuses the check; any other is accepted.
    case "GET settings/smtp":
    case "PUT settings/smtp":
    case "DELETE settings/smtp":
    case "POST settings/smtp/test": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
        return true;
      }
      if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.mailSettings);
        return true;
      }
      if (is("DELETE", ctx)) {
        ctx.state.mailSettings = { source: "host", host: "smtp.mock.felis.local", port: 587, from: "noreply@mock.felis.local", username: "noreply@mock.felis.local", password_set: true, require_tls: true, max_per_hour: 0, revision: "" };
        sendJSON(ctx.res, 200, ctx.state.mailSettings);
        return true;
      }
      const body = await readJSON<MailSettingsRequest>(ctx.req);
      if (![25, 465, 587, 2525].includes(body.port) || !body.host || !/^[^\s@]+@[^\s@]+$/.test(body.from)) {
        sendError(ctx.res, 400, "bad_request", "invalid relay");
      } else if (body.host.startsWith("smtp.fail")) {
        sendError(ctx.res, 400, "smtp_auth", "the relay refused the user name or password");
      } else if (is("POST", ctx)) {
        sendJSON(ctx.res, 200, { sent_to: ctx.account.email || body.from });
      } else if (body.revision !== ctx.state.mailSettings.revision) {
        sendError(ctx.res, 409, "mail_settings_changed", "the mail settings changed; reload before saving");
      } else {
        if (body.password !== undefined) ctx.state.mailPassword = body.password;
        const { password: _password, revision: _revision, ...relay } = body;
        ctx.state.mailSettings = { ...relay, source: "panel", password_set: ctx.state.mailPassword !== "", revision: createHash("sha256").update(JSON.stringify(relay)).digest("hex") };
        sendJSON(ctx.res, 200, ctx.state.mailSettings);
      }
      return true;
    }
    // Defaults until a save; a manual keep of 0 is refused.
    case "GET settings/archive-policy":
    case "PUT settings/archive-policy":
    case "DELETE settings/archive-policy": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
      } else if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.archivePolicy);
      } else if (is("DELETE", ctx)) {
        ctx.state.archivePolicy = hostArchivePolicy();
        sendJSON(ctx.res, 200, ctx.state.archivePolicy);
      } else {
        const { revision, source: _source, ...saved } = await readJSON<ArchivePolicySettings>(ctx.req);
        if (revision !== ctx.state.archivePolicy.revision) sendError(ctx.res, 409, "conflict", "the retention changed; reload before saving");
        else if (!(saved.manualKeep >= 1 && saved.manualKeep <= 100)) sendError(ctx.res, 400, "bad_request", "manualKeep must be 1–100");
        else {
          ctx.state.archivePolicy = { ...saved, source: "panel", revision: createHash("sha256").update(JSON.stringify(saved)).digest("hex") };
          sendJSON(ctx.res, 200, ctx.state.archivePolicy);
        }
      }
      return true;
    }
    case "GET settings/heartbeat":
    case "PUT settings/heartbeat":
    case "DELETE settings/heartbeat": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
      } else if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.heartbeat);
      } else if (is("DELETE", ctx)) {
        ctx.state.heartbeat = { set: false, display: "", revision: "" };
        sendJSON(ctx.res, 200, ctx.state.heartbeat);
      } else {
        const body = await readJSON<{ url: string; revision: string }>(ctx.req);
        let parsed: URL | null = null;
        try { parsed = new URL(body.url); } catch { parsed = null; }
        if (body.revision !== ctx.state.heartbeat.revision) sendError(ctx.res, 409, "conflict", "the heartbeat changed; reload before saving");
        else if (!parsed || (parsed.protocol !== "https:" && parsed.protocol !== "http:")) sendError(ctx.res, 400, "bad_heartbeat_url", "the heartbeat must be an http(s) URL");
        else {
          ctx.state.heartbeat = { set: true, display: `${parsed.protocol}//${parsed.host}`, revision: createHash("sha256").update(body.url).digest("hex") };
          sendJSON(ctx.res, 200, ctx.state.heartbeat);
        }
      }
      return true;
    }
    case "GET settings/failover":
    case "PUT settings/failover": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
      } else if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.failoverPolicy);
      } else {
        const body = await readJSON<Omit<FailoverPolicySettings, "managed">>(ctx.req);
        if (body.revision !== ctx.state.failoverPolicy.revision) sendError(ctx.res, 409, "conflict", "the failover setting changed; reload before saving");
        else if (body.afterMinutes < 10 || body.afterMinutes > 1440 || (body.replicaMinutes !== 0 && (body.replicaMinutes < 15 || body.replicaMinutes > 1440))) sendError(ctx.res, 400, "bad_request", "invalid failover setting");
        else {
          const { revision: _revision, ...policy } = body;
          ctx.state.failoverPolicy = { ...policy, managed: true, revision: createHash("sha256").update(JSON.stringify(policy)).digest("hex") };
          sendJSON(ctx.res, 200, ctx.state.failoverPolicy);
        }
      }
      return true;
    }
    case "GET settings/auth-sources":
    case "PUT settings/auth-sources":
    case "POST settings/auth-sources/test": {
      if (!isOwner(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "Owner account required");
        return true;
      }
      if (is("GET", ctx)) {
        sendJSON(ctx.res, 200, ctx.state.authSources);
      } else if (is("PUT", ctx)) {
        const body = await readJSON<{ sources: AuthSourceConfig[]; revision: string }>(ctx.req);
        if (body.revision !== ctx.state.authSources.revision) {
          sendError(ctx.res, 409, "auth_sources_changed", "authentication sources changed; reload before saving");
        } else if (ctx.state.authSources.retired.some((r) => body.sources.some((s) => s.tag === r.tag && s.url !== r.url))) {
          sendError(ctx.res, 409, "auth_source_tag_retired", "a deleted source's ID can be reused only with its endpoint");
        } else {
          // A source left out is deleted and its tag retired; adding it back with the same endpoint restores it.
          const { sources, retired } = ctx.state.authSources;
          const kept = retired.filter((r) => !body.sources.some((s) => s.tag === r.tag));
          const deleted = sources.filter((source) => !body.sources.some((s) => s.tag === source.tag)).map(({ tag, url }) => ({ tag, url }));
          ctx.state.authSources = { sources: body.sources, retired: [...kept, ...deleted], revision: createHash("sha256").update(JSON.stringify(body.sources)).digest("hex"), managed: true };
          sendJSON(ctx.res, 200, ctx.state.authSources);
        }
      } else {
        await readJSON<AuthSourceConfig>(ctx.req);
        sendJSON(ctx.res, 200, { ok: true, status: 204, elapsed_ms: 20 });
      }
      return true;
    }
    case "GET account/link/sources":
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      sendJSON(ctx.res, 200, { sources: [{ tag: "mojang", prefix: "", lookup_available: true }, ...ctx.state.authSources.sources.filter((source) => source.enabled).map((source) => ({ tag: source.tag, prefix: source.prefix, lookup_available: !!source.api_url || source.url.endsWith("/sessionserver/session/minecraft/hasJoined") }))] });
      return true;
    case "GET platform/db-backup": {
      if (ctx.account.role !== "owner") {
        sendError(ctx.res, 403, "forbidden", "owner account required");
        return true;
      }
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
          daily_at: at.toISOString(),
        },
        stale: false,
        max_age_seconds: 26 * 3600,
      });
      return true;
    }
    case "GET updates/report": {
      if (ctx.account.role !== "owner") {
        sendError(ctx.res, 403, "forbidden", "owner account required");
        return true;
      }
      // one update waiting, one feed unreachable, one install unreadable
      sendJSON(ctx.res, 200, {
        report: {
          checked_at: new Date(Date.now() - 7 * 3600 * 1000).toISOString(),
          felis: "v0.4.0",
          components: [
            { name: "felis-api", current: "v0.4.0", latest: "v0.5.1", state: "available", selector: "panel" },
            { name: "velocity", current: "3.4.0-SNAPSHOT+b528", state: "current", selector: "velocity" },
            { name: "k3s", current: "v1.36.2+k3s1", state: "current", selector: "k3s" },
            { name: "cloudflared", current: "2026.6.1", latest: "2026.9.0", state: "available", selector: "cloudflared" },
            { name: "jre", current: "21.0.8+9", state: "unknown", selector: "jre", error: "adoptium: GET https://api.adoptium.net/v3/assets/latest/21/hotspot: context deadline exceeded" },
            { name: "postgresql", current: "13.22", state: "current", selector: "postgres", note: "PostgreSQL 13 reached its end of life on 2025-11-13; plan a major upgrade (https://docs.felismc.com/en/operations/upgrade#postgresql-major)" },
          ],
        },
        stale: false,
        max_age_seconds: 26 * 3600,
      });
      return true;
    }
    case "GET updates/window":
      if (ctx.account.role !== "owner") {
        sendError(ctx.res, 403, "forbidden", "owner account required");
        return true;
      }
      sendJSON(ctx.res, 200, ctx.state.updateWindow);
      return true;
    case "PUT updates/window": {
      if (ctx.account.role !== "owner") {
        sendError(ctx.res, 403, "forbidden", "owner account required");
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
      // Admin only, as the real adminOnly gate.
      if (!isAdmin(ctx.account.role)) {
        sendError(ctx.res, 403, "forbidden", "admin account required");
        return true;
      }
      sendJSON(ctx.res, 200, { servers: fleetView(ctx.state, ctx.account) });
      return true;
    case "GET backups":
      {
        // Admins see every archive, users only the worlds they once owned; paged, newest first.
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
      // Replacing a verified address signs the other devices out; a proven code counts as a re-auth.
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
      // No migration is pending until one is started in-game.
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
      if (ctx.method === "DELETE" && ctx.parts[2] === "backups" && ctx.parts[3] && ctx.parts.length === 4) {
        // Outside the caller's scope is 404 no_backup; a restore reading it is 409 restore_in_progress.
        const b = ctx.state.backups.find((x) => x.id === ctx.parts[3] && x.status === "present");
        if (!b || !(isAdmin(ctx.account.role) || b.former_owner === ctx.account.id)) {
          sendError(ctx.res, 404, "no_backup", "no matching backup exists");
          return true;
        }
        const reading = (ctx.state.jobs[b.server_name] ?? []).some(
          (j) => (j.kind === "restore" && j.state === "running") || (j.then_restore === "pending" && j.restore_backup_id === b.id),
        );
        if (reading) {
          sendError(ctx.res, 409, "restore_in_progress", "a restore is running on this backup's server and may be reading it; delete it once the restore finishes");
          return true;
        }
        b.status = "expired";
        sendJSON(ctx.res, 200, { id: b.id, status: "expired" });
        return true;
      }
      if (ctx.method === "DELETE" && ctx.parts[2] === "account" && ctx.parts[3] === "passkey" && ctx.parts[4] === "credentials" && ctx.parts[5]) {
        // Re-auth is checked before the passkey lookup.
        if (refusedForReauth(ctx)) return true;
        const id = ctx.parts[5];
        const list = ctx.state.passkeys[ctx.account.id] ?? [];
        const idx = list.findIndex((k) => k.id === id);
        if (idx < 0) {
          sendError(ctx.res, 404, "not_found", "passkey not found");
          return true;
        }
        // The only passkey of an account without a verified email stays; it is the sole way to sign in.
        if (list.length === 1 && !ctx.account.emailVerified) {
          sendError(ctx.res, 409, "last_passkey", "this is your only passkey and your email is not verified");
          return true;
        }
        list.splice(idx, 1);
        // Removing a passkey signs the other devices out.
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

  if (!isOwner(ctx.account.role)) {
    sendError(ctx.res, 403, "forbidden", "owner account required");
    return true;
  }

  const userIdOrAction = ctx.parts[3];

  if (is("GET", ctx) && !userIdOrAction) {
    const url = new URL(ctx.req.url ?? "/", "http://localhost");
    const search = url.searchParams.get("search")?.toLowerCase() || "";
    const page = parseInt(url.searchParams.get("page") || "1", 10);
    const limit = parseInt(url.searchParams.get("limit") || "10", 10);

    const allUsers = Object.values(ctx.state.accounts).map((acc) => {
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

  if (userIdOrAction) {
    const rawId = userIdOrAction;
    const accountKey = rawId.startsWith("mock-") ? rawId.substring(5) : rawId;
    const acc = ctx.state.accounts[accountKey];

    if (!acc) {
      sendError(ctx.res, 404, "not_found", "user not found");
      return true;
    }

    const subAction = ctx.parts[4];

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

    // Returns the user's own quota, or none when the default applies.
    if (is("GET", ctx) && subAction === "quotas") {
      sendJSON(ctx.res, 200, acc.quota ? { ...acc.quota, custom: true } : { user_id: acc.id, custom: false });
      return true;
    }

    if (is("PUT", ctx) && subAction === "quotas") {
      const body = await readJSON<QuotaInput>(ctx.req);
      const caps = [body.max_servers, body.max_cpu_milli, body.max_memory_mb, body.max_storage_gb];
      if (!caps.every((v) => v == null || (Number.isInteger(v) && v >= 0 && v <= 2147483647))) {
        sendError(ctx.res, 400, "invalid_quota", "each cap must be a whole number from 0 to 2147483647, or null for unlimited");
        return true;
      }
      acc.quota = {
        user_id: acc.id,
        ...(body.max_servers != null && { max_servers: body.max_servers }),
        ...(body.max_cpu_milli != null && { max_cpu_milli: body.max_cpu_milli }),
        ...(body.max_memory_mb != null && { max_memory_mb: body.max_memory_mb }),
        ...(body.max_storage_gb != null && { max_storage_gb: body.max_storage_gb }),
      };
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, { ...acc.quota, custom: true });
      return true;
    }

    if (is("DELETE", ctx) && subAction === "quotas") {
      acc.quota = undefined;
      acc.updated_at = new Date().toISOString();
      sendJSON(ctx.res, 200, { user_id: acc.id, custom: false });
      return true;
    }

    if (is("GET", ctx) && subAction === "sessions") {
      sendJSON(ctx.res, 200, { sessions: accountSessions(acc) });
      return true;
    }

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

    if (is("DELETE", ctx) && subAction === "sessions" && !ctx.parts[5]) {
      acc.sessions = [];
      sendJSON(ctx.res, 200, { ok: true });
      return true;
    }

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

  if (is("GET", ctx) && ctx.parts.length === 3) {
    sendJSON(ctx.res, 200, { images: ctx.state.images });
    return true;
  }

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

    sendJSON(ctx.res, 202, newBuild);
    return true;
  }

  if (is("GET", ctx) && ctx.parts[3] === "build" && ctx.parts.length === 4) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    // Mirrors build.PGStore.ListBuilds: newest first; a query matches the id or status exactly, or part of the ref.
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

  // Only bld-1 (passed) and bld-2 (blocked) have a kept scan.
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

// Mirrors submit.PGStore.PageSubmissions: counts cover the whole scope; status and query narrow the rows, newest first.
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

  if (isMeSubmissions && is("GET", ctx) && ctx.parts.length === 4) {
    const userSubs = ctx.state.submissions.filter((s) => s.submitted_by === ctx.account.email);
    sendJSON(ctx.res, 200, submissionPageOf(ctx, userSubs));
    return true;
  }

  if (isMeSubmissions && is("GET", ctx) && ctx.parts.length === 5 && ctx.parts[4] === "limits") {
    sendJSON(ctx.res, 200, { max_context_bytes: MOCK_CONTEXT_MAX });
    return true;
  }

  if (isMeSubmissions && is("DELETE", ctx) && ctx.parts.length === 5) {
    const id = ctx.parts[4];
    const idx = ctx.state.submissions.findIndex((s) => s.id === id && s.submitted_by === ctx.account.email);
    if (idx < 0) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    const sub = ctx.state.submissions[idx];
    if (sub.status !== "pending_review") {
      sendError(ctx.res, 409, "already_reviewed", "submission has already been reviewed");
      return true;
    }
    ctx.state.submissions.splice(idx, 1);
    contextBlobs.delete(id);
    stagedParts.delete(id);
    sendJSON(ctx.res, 200, sub);
    return true;
  }

  // Chunked upload: parts stay in memory and the staged length is the resume point.
  if (isMeSubmissions && ctx.parts[5] === "context" && ctx.parts[6] === "upload") {
    const id = ctx.parts[4];
    const sub = ctx.state.submissions.find((s) => s.id === id && s.submitted_by === ctx.account.email);
    if (!sub) {
      sendError(ctx.res, 404, "not_found", "submission not found");
      return true;
    }
    if (sub.status !== "pending_review") {
      sendError(ctx.res, 409, "already_reviewed", "submission has already been reviewed");
      return true;
    }
    const staged = stagedParts.get(id) ?? Buffer.alloc(0);
    const progress = (received: number) => ({
      received,
      part_max_bytes: MOCK_PART_MAX,
      max_context_bytes: MOCK_CONTEXT_MAX,
    });
    if (is("GET", ctx) && ctx.parts.length === 7) {
      sendJSON(ctx.res, 200, progress(staged.length));
      return true;
    }
    if (is("PUT", ctx) && ctx.parts.length === 7) {
      const raw = new URL(ctx.req.url ?? "/", "http://localhost").searchParams.get("offset");
      const offset = raw === null || !/^\d+$/.test(raw) ? NaN : Number(raw);
      if (Number.isNaN(offset)) {
        sendError(ctx.res, 400, "bad_request", "offset must be the byte position the part starts at");
        return true;
      }
      if (offset !== 0 && offset !== staged.length) {
        sendError(ctx.res, 409, "upload_offset_mismatch", `the upload holds ${staged.length} bytes; send the part that starts there`);
        return true;
      }
      const chunks: Buffer[] = [];
      for await (const chunk of ctx.req) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
      const part = Buffer.concat(chunks);
      if (part.length > MOCK_PART_MAX) {
        sendError(ctx.res, 413, "part_too_large", "an upload part exceeds the part size limit");
        return true;
      }
      if (offset === 0 && (part[0] !== 0x1f || part[1] !== 0x8b)) {
        sendError(ctx.res, 400, "bad_request", "context must be a gzip-compressed tarball");
        return true;
      }
      const next = Buffer.concat([offset === 0 ? Buffer.alloc(0) : staged, part]);
      stagedParts.set(id, next);
      // The pause lets the progress bar be seen and paused.
      await new Promise((r) => setTimeout(r, 250));
      sendJSON(ctx.res, 200, progress(next.length));
      return true;
    }
    if (is("POST", ctx) && ctx.parts.length === 8 && ctx.parts[7] === "complete") {
      if (!stagedParts.has(id)) {
        sendError(ctx.res, 400, "bad_request", "no upload in progress for this submission; send its parts first");
        return true;
      }
      contextBlobs.set(id, staged);
      sub.context_sha256 = sha256Hex(staged);
      stagedParts.delete(id);
      sendJSON(ctx.res, 200, sub);
      return true;
    }
  }
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

  // Reviewer download; carries the digest the approval must name.
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

  if (isAdminSubmissions && is("GET", ctx) && ctx.parts.length === 3) {
    if (!isAdmin(ctx.account.role)) {
      sendError(ctx.res, 403, "forbidden", "admin account required");
      return true;
    }
    sendJSON(ctx.res, 200, submissionPageOf(ctx, ctx.state.submissions));
    return true;
  }

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
  if (serverInfo.reaperExempt && !isAdmin(ctx.account.role)) {
    sendError(ctx.res, 400, "bad_name", "system service is reserved for staff");
    return true;
  }
  if (is("GET", ctx) && ctx.parts[4] === "status") {
    sendJSON(ctx.res, 200, statusView(serverInfo, ctx.account));
    return true;
  }
  if (!canSee(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not visible to this account");
    return true;
  }

  if (is("GET", ctx) && ctx.parts[4] === "usage") {
    if (!canManage(ctx.account, serverInfo)) sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    else sendJSON(ctx.res, 200, mockUsage(serverInfo));
    return true;
  }
  if (is("GET", ctx) && ctx.parts[4] === "startup-settings" && ctx.parts.length === 5) {
    if (!canManage(ctx.account, serverInfo)) sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    else {
      const settings = mockStartupSettings(ctx.state, serverInfo.name);
      ctx.res.setHeader("Cache-Control", "no-store");
      sendJSON(ctx.res, 200, { ...settings, env: settings.env.filter((entry) => !reservedStartupEnv(entry.name)) });
    }
    return true;
  }
  if (is("PATCH", ctx) && !ctx.parts[4]) {
    if (!isAdmin(ctx.account.role)) sendError(ctx.res, 403, "forbidden", "admin account required");
    else await patchMockServer(ctx, serverInfo, false);
    return true;
  }
  if (is("GET", ctx) && ctx.parts[4] === "console" && ctx.parts[5] === "history" && ctx.parts.length === 6) {
    return handleConsoleHistoryMock(ctx, serverInfo);
  }
  if (is("GET", ctx) && ctx.parts[4] === "console") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    streamConsole(ctx.req, ctx.res, serverInfo);
    return true;
  }
  if (is("POST", ctx) && (ctx.parts[4] === "wake" || ctx.parts[4] === "restart")) {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    if (serverInfo.retiring) {
      sendError(ctx.res, 409, "server_retiring", "this server is being given up or deleted; cancel that first");
      return true;
    }
    if (mockDiskUsage(serverInfo).diskFull) {
      sendError(ctx.res, 409, "disk_full", "remove files and wait for a new measurement below the disk quota");
      return true;
    }
    if (ctx.parts[4] === "restart" && serverInfo.phase !== "Running") {
      sendError(ctx.res, 409, "not_running", "start the server before restarting it");
      return true;
    }
    Object.assign(serverInfo, { autoStop: undefined, crashStopped: undefined, recentCrashes: undefined });
    setPhase(serverInfo, "Starting");
    sendJSON(ctx.res, 202, { name: serverInfo.name, desiredState: "Running" });
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "emergency-stop") {
    if (!ctx.account || !isOwner(ctx.account.role)) { sendError(ctx.res, 403, "forbidden", "owner access required"); return true; }
    const body = await readJSON(ctx.req) as { confirm?: string };
    if (body.confirm !== serverInfo.name) { sendError(ctx.res, 400, "bad_request", "type the server name"); return true; }
    setPhase(serverInfo, "Stopped");
    sendJSON(ctx.res, 202, { name: serverInfo.name, desiredState: "Stopped" });
    return true;
  }
  if (is("POST", ctx) && (ctx.parts[4] === "stop" || ctx.parts[4] === "kill")) {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    setPhase(serverInfo, "Stopped");
    sendJSON(ctx.res, 202, { name: serverInfo.name, desiredState: "Stopped" });
    return true;
  }
  if (is("POST", ctx) && ctx.parts[4] === "command") {
    if (!canManage(ctx.account, serverInfo)) {
      sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
      return true;
    }
    await handleCommandMock(ctx, serverInfo);
    return true;
  }
  if (ctx.parts[4] === "retirement" && (is("PUT", ctx) || is("DELETE", ctx))) {
    await handleRetirementMock(ctx, serverInfo);
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
  if (ctx.parts.slice(4).join("/") === "config/paper-global") {
    return handlePaperGlobalMock(ctx, serverInfo);
  }
  if (ctx.parts[4] === "files" || ctx.parts[4] === "file" || ctx.parts[4] === "extensions") {
    return handleFilesMock(ctx, serverInfo);
  }

  return false;
}


interface MockFileNode {
  is_dir: boolean;
  data: Buffer;
  mod_time: string;
  extension_metadata?: ExtensionMetadata[];
}
type MockTree = Map<string, MockFileNode>;

const MOCK_MAX_WRITE = 5 * 1024 * 1024;
const MOCK_MAX_READ = MOCK_MAX_WRITE;
const MOCK_MAX_UPLOAD = 64 * 1024 * 1024;
const MOCK_SECRET_CONFIG = "config/paper-global.yml";
const MOCK_MANAGED = new Set([MOCK_SECRET_CONFIG, "config", "server.properties"]);
const MOCK_LOG_FILE_IDS = new WeakMap<MockFileNode, number>();
let mockLogFileID = 0;
const MOCK_REDACTED_VALUE = "<redacted by felis>";
const MOCK_SECRET_PROPERTY = /^([ \t\f]*(rcon\.password|forwarding-secrets)=)([^\r\n]*)/gm;

function redactMockProperties(content: Buffer): Buffer {
  return Buffer.from(content.toString().replace(MOCK_SECRET_PROPERTY, `$1${MOCK_REDACTED_VALUE}`));
}

function preserveMockProperties(content: Buffer, original: Buffer): Buffer {
  const values = new Map<string, string>([...original.toString().matchAll(MOCK_SECRET_PROPERTY)].map((match) => [match[2], match[3]]));
  return Buffer.from(content.toString().replace(MOCK_SECRET_PROPERTY, (line, prefix: string, key: string, value: string) =>
    value === MOCK_REDACTED_VALUE && values.has(key) ? prefix + values.get(key) : line));
}

// seedFiles is a Paper server's volume after a few days of play: the configs an
// owner edits, plugin jars, a world and its logs. Sizes are real-looking, the
// binary content is zeros.
function seedFiles(serverName: string): MockTree {
  const tree: MockTree = new Map();
  const ago = (hours: number) => new Date(Date.now() - hours * 3_600_000).toISOString();
  const dir = (p: string, hours: number) => tree.set(p, { is_dir: true, data: Buffer.alloc(0), mod_time: ago(hours) });
  const file = (p: string, content: string | number, hours: number) =>
    tree.set(p, {
      is_dir: false,
      data: typeof content === "string" ? Buffer.from(content) : Buffer.alloc(content),
      mod_time: ago(hours),
    });
  dir("", 0);
  file(
    "server.properties",
    `#Minecraft server properties\nmotd=${serverName}\nmax-players=${serverName === "login" ? "-1" : "20"}\nonline-mode=false\ndifficulty=normal\ngamemode=survival\npvp=true\nview-distance=10\nsimulation-distance=10\nentity-broadcast-range-percentage=100\nwhite-list=false\nspawn-protection=16\nserver-port=25565\nenable-rcon=true\nrcon.port=25575\nrcon.password=${MOCK_REDACTED_VALUE}\n${serverName === "login" ? `bungeecord=false\nbungee-guard=false\nvelocity-modern=true\nforwarding-secrets=${MOCK_REDACTED_VALUE}\n` : ""}`,
    30,
  );
  file("eula.txt", "eula=true\n", 400);
  file("ops.json", "[]\n", 50);
  file("whitelist.json", '[\n  {\n    "uuid": "069a79f4-44e9-4726-a5be-fca90e38aaf5",\n    "name": "Notch"\n  }\n]\n', 50);
  file("banned-players.json", "[]\n", 50);
  file("bukkit.yml", "settings:\n  allow-end: true\n  warn-on-overload: true\nspawn-limits:\n  monsters: 70\n", 300);
  file("spigot.yml", "settings:\n  debug: false\n  restart-on-crash: true\n", 300);
  dir("config", 300);
  file(MOCK_SECRET_CONFIG, "# Global configuration\nproxies:\n  velocity:\n    enabled: true\n    online-mode: true\n    secret: mock-forwarding-secret\nchunk-loading-advanced:\n  auto-config-send-distance: true\n  player-max-concurrent-chunk-generates: 0\n  player-max-concurrent-chunk-loads: 0\nchunk-loading-basic:\n  player-max-chunk-generate-rate: -1.0\n  player-max-chunk-load-rate: 100.0\n  player-max-chunk-send-rate: 75.0\ncollisions:\n  enable-player-collisions: true\n  send-full-pos-for-hard-colliding-entities: true\ncommands:\n  suggest-player-names-when-null-tab-completions: true\nconsole:\n  enable-brigadier-completions: true\n  enable-brigadier-highlighting: true\nmessages:\n  use-display-name-in-quit-message: false\n  no-permission: '<red>You do not have permission.</red>'\n  kick:\n    connection-throttle: 'Connection throttled. Please wait.'\nplayer-auto-save:\n  max-per-tick: -1\n  rate: -1\nscoreboards:\n  save-empty-scoreboard-teams: true\n  track-plugin-scoreboards: false\nwatchdog:\n  early-warning-delay: 10000\n  early-warning-every: 5000\n", 30);
  file("config/paper-world-defaults.yml", "entities:\n  spawning:\n    per-player-mob-spawns: true\n", 300);
  file("purpur.yml", "settings:\n  use-alternate-keepalive: false\n  server-mod-name: Purpur\n  messages:\n    afk-broadcast-away: '%s is now AFK'\nworld-settings:\n  default:\n    gameplay-mechanics:\n      player:\n        idle-timeout:\n          kick-if-idle: false\n", 300);
  dir("plugins", 72);
  file("plugins/LuckPerms-Bukkit-5.4.141.jar", 1_380_214, 72);
  file("plugins/EssentialsX-2.20.1.jar", 3_120_876, 70);
  file("plugins/Chunky-Bukkit-1.4.28.jar", 412_553, 12);
  tree.get("plugins/LuckPerms-Bukkit-5.4.141.jar")!.extension_metadata = [{ id: "LuckPerms", name: "LuckPerms", version: "5.4.141", authors: ["Luck"], loaders: ["bukkit", "paper"] }];
  tree.get("plugins/EssentialsX-2.20.1.jar")!.extension_metadata = [{ id: "Essentials", name: "EssentialsX", version: "2.20.1", authors: ["EssentialsX Team"], loaders: ["bukkit"] }];
  tree.get("plugins/Chunky-Bukkit-1.4.28.jar")!.extension_metadata = [{ id: "Chunky", name: "Chunky", version: "1.4.28", authors: ["pop4959"], loaders: ["bukkit"] }];
  file("plugins/broken.jar", "not a ZIP archive", 5);
  dir("plugins/LuckPerms", 72);
  file("plugins/LuckPerms/config.yml", "server: global\nstorage-method: h2\n", 72);
  dir("plugins/Essentials", 70);
  file("plugins/Essentials/config.yml", "ops-name-color: '4'\nnickname-prefix: '~'\n", 70);
  dir("mods", 24);
  file("mods/example-mod-1.0.jar.disabled", 1024, 24);
  tree.get("mods/example-mod-1.0.jar.disabled")!.extension_metadata = [{ id: "example_mod", name: "Example Mod", version: "1.0", authors: ["Example Author"], loaders: ["fabric", "quilt"] }];
  file("mods/example-pack-1.0.jar", 2048, 24);
  tree.get("mods/example-pack-1.0.jar")!.extension_metadata = [
    { id: "example_core", name: "Example Core", version: "1.0", authors: ["Example Team"], loaders: ["forge", "neoforge"] },
    { id: "example_extras", name: "Example Extras", version: "1.0", authors: ["Example Team"], loaders: ["forge", "neoforge"] },
  ];
  dir("world", 2);
  file("world/level.dat", 2_431, 2);
  file("world/session.lock", 3, 2);
  dir("world/region", 2);
  file("world/region/r.0.0.mca", 8_392_704, 2);
  file("world/region/r.-1.0.mca", 5_246_976, 3);
  file("world/region/r.0.-1.mca", 6_295_552, 26);
  dir("world_nether", 26);
  dir("world_the_end", 140);
  dir("logs", 2);
  file("logs/latest.log", `[12:00:00] [Server thread/INFO]: Starting minecraft server version 1.21.1\n[12:00:04] [Server thread/INFO]: Done (3.912s)! For help, type "help"\n`, 2);
  return tree;
}

// emptyVolume is a new server's world volume: nothing in it until its first start
// or an upload.
function emptyVolume(): MockTree {
  return new Map([["", { is_dir: true, data: Buffer.alloc(0), mod_time: new Date().toISOString() }]]);
}

function filesOf(state: MockState, serverName: string): MockTree {
  let tree = state.files[serverName];
  if (!tree) {
    tree = seedFiles(serverName);
    state.files[serverName] = tree;
  }
  return tree;
}

function paperGlobalScalar(field: ConfigFieldSpec, value: unknown): boolean {
  if (field.type === "boolean") return typeof value === "boolean";
  if (field.type === "text") return typeof value === "string";
  if (field.type === "integer") return typeof value === "bigint" && Number.isFinite(Number(value));
  return (typeof value === "number" || typeof value === "bigint") && Number.isFinite(Number(value));
}

async function handlePaperGlobalMock(ctx: SessionContext, server: MockServer): Promise<boolean> {
  if (!is("GET", ctx) && !is("PATCH", ctx)) return false;
  ctx.res.setHeader("Cache-Control", "no-store");
  const fail = (status: number, code: string): true => { sendError(ctx.res, status, code, "the protected configuration could not be read or saved"); return true; };
  if (!canManage(ctx.account, server)) return fail(403, "forbidden");
  const running = server.phase === "Running" && server.desiredState === "Running";
  if (!running && (server.ready || server.desiredState !== "Stopped" || ["Starting", "Stopping"].includes(server.phase))) return fail(409, "server_transitioning");
  const jobs = ctx.state.jobs[server.name] ?? [];
  if (jobs.some((job) => job.then_restore === "pending" || (job.state === "running" && ["backup", "restore", "migration", "reap"].includes(job.kind)))) return fail(409, "maintenance_in_progress");
  let patch: Record<string, unknown> | undefined;
  let expected: string | undefined;
  if (is("PATCH", ctx)) {
    const bytes = await readRawBody(ctx.req);
    if (bytes.length > 64 * 1024) return fail(413, "too_large");
    let body: unknown;
    try { body = JSON.parse(bytes.toString()); } catch { return fail(400, "bad_request"); }
    if (!body || typeof body !== "object" || Array.isArray(body)) return fail(400, "bad_request");
    const request = body as Record<string, unknown>;
    if (Object.keys(request).some((key) => !["values", "expect_sha256"].includes(key)) || typeof request.expect_sha256 !== "string" || !/^[0-9a-f]{64}$/.test(request.expect_sha256) ||
      !request.values || typeof request.values !== "object" || Array.isArray(request.values)) return fail(400, "bad_request");
    patch = request.values as Record<string, unknown>;
    expected = request.expect_sha256;
    for (const [key, value] of Object.entries(patch)) {
      const field = PAPER_GLOBAL_FIELDS.find((item) => item.path.join(".") === key);
      const matches = field?.type === "boolean" ? typeof value === "boolean" : field?.type === "text" ? typeof value === "string" : typeof value === "number";
      if (!field || !matches || configProblem(field, String(value))) return fail(400, "bad_request");
    }
    if (!running && (jobs.some((job) => job.state === "running" && job.kind !== "export_backup") || (ctx.state.fileOps[server.name] ?? []).some((op) => op.state === "running"))) return fail(409, "maintenance_in_progress");
  }
  const tree = filesOf(ctx.state, server.name);
  const node = tree.get(MOCK_SECRET_CONFIG);
  if (!node) return fail(404, "not_found");
  if (node.is_dir) return fail(400, "bad_path");
  if (node.data.length > MOCK_MAX_READ) return fail(413, "too_large");
  if (expected && mockSha(node.data) !== expected) return fail(409, "file_changed");
  try {
    let text = new TextDecoder("utf-8", { fatal: true }).decode(node.data);
    let document = readYamlConfig(text, "1.2");
    for (const key of Object.keys(patch ?? {})) {
      const field = PAPER_GLOBAL_FIELDS.find((item) => item.path.join(".") === key)!;
      if (!paperGlobalScalar(field, getYamlScalar(document, field.path))) return fail(400, "bad_path");
    }
    for (const [key, value] of Object.entries(patch ?? {})) {
      const field = PAPER_GLOBAL_FIELDS.find((item) => item.path.join(".") === key)!;
      text = setYamlScalar(text, field.path, field.type === "integer" ? BigInt(value as number) : value as string | number | boolean, "1.2");
    }
    const content = Buffer.from(text);
    if (content.length > MOCK_MAX_WRITE) return fail(413, "too_large");
    if (text !== node.data.toString()) tree.set(MOCK_SECRET_CONFIG, { is_dir: false, data: content, mod_time: new Date().toISOString() });
    document = readYamlConfig(text, "1.2");
    const values: Record<string, boolean | number | string> = {};
    for (const field of PAPER_GLOBAL_FIELDS) {
      const value = getYamlScalar(document, field.path);
      if (paperGlobalScalar(field, value)) values[field.path.join(".")] = typeof value === "bigint" ? Number(value) : value as boolean | number | string;
    }
    sendJSON(ctx.res, 200, { values, sha256: mockSha(content) });
    return true;
  } catch (error) {
    return fail(error instanceof YamlConfigParseError || error instanceof TypeError ? 400 : 500, error instanceof YamlConfigParseError || error instanceof TypeError ? "bad_path" : "internal");
  }
}

function handleConsoleHistoryMock(ctx: SessionContext, server: MockServer): true {
  ctx.res.setHeader("Cache-Control", "no-store");
  const fail = (status: number, code: string, message: string): true => { sendError(ctx.res, status, code, message); return true; };
  if (!canManage(ctx.account, server)) return fail(403, "forbidden", "server is not owned by this account");
  const encoded = new URL(ctx.req.url!, "http://localhost").searchParams.get("cursor") ?? "";
  if (Buffer.byteLength(encoded) > 256) return fail(400, "bad_path", "invalid log cursor");
  if (server.desiredState !== "Stopped" || server.ready || ["Running", "Starting", "Stopping"].includes(server.phase)) {
    return fail(409, "server_transitioning", "saved logs require a fully stopped server");
  }
  if ((ctx.state.jobs[server.name] ?? []).some((job) => job.then_restore === "pending" || (job.state === "running" && ["backup", "restore", "migration", "reap"].includes(job.kind)))) {
    return fail(409, "maintenance_in_progress", "another operation holds this server's files");
  }
  let cursor: [string, number, number, string, number] | undefined;
  if (encoded) {
    try {
      const raw = Buffer.from(encoded, "base64url");
      const parsed: unknown = JSON.parse(raw.toString());
      if (raw.toString("base64url") !== encoded || !Array.isArray(parsed) || parsed.length !== 5 ||
        typeof parsed[0] !== "string" || !Number.isSafeInteger(parsed[1]) || !Number.isSafeInteger(parsed[2]) || typeof parsed[3] !== "string" ||
        !Number.isSafeInteger(parsed[4]) || parsed[1] < 1 || parsed[2] < 0 || parsed[4] < 0 || parsed[4] > parsed[2]) throw new Error("invalid cursor");
      cursor = parsed as [string, number, number, string, number];
    } catch {
      return fail(400, "bad_path", "invalid log cursor");
    }
  }
  const node = filesOf(ctx.state, server.name).get("logs/latest.log");
  if (node?.is_dir) return fail(400, "bad_path", "the log must be a regular file");
  if (!node && cursor) return fail(409, "file_changed", "the log file changed; refresh its latest page");
  if (node && !MOCK_LOG_FILE_IDS.has(node)) MOCK_LOG_FILE_IDS.set(node, ++mockLogFileID);
  if (cursor && (cursor[0] !== server.name || cursor[1] !== MOCK_LOG_FILE_IDS.get(node!) || cursor[2] !== node!.data.length || cursor[3] !== node!.mod_time)) {
    return fail(409, "file_changed", "the log file changed; refresh its latest page");
  }
  const bytes = node?.data ?? Buffer.alloc(0);
  const before = cursor?.[4] ?? bytes.length;
  let start = Math.max(0, before - 256 * 1024);
  let content = bytes.subarray(start, before);
  let byte_limited = false;
  if (content.length) {
    const completeStart = start === 0 || bytes[start - 1] === 10 || (bytes[start - 1] === 13 && content[0] !== 10);
    let end = content.length;
    if (content[end - 1] === 10) { end--; if (end > 0 && content[end - 1] === 13) end--; }
    else if (content[end - 1] === 13) end--;
    let lines = 1;
    let boundary = -1;
    for (let i = end - 1; i >= 0; i--) {
      if (content[i] !== 10 && content[i] !== 13) continue;
      if (lines++ === 200) { boundary = i + 1; break; }
      if (content[i] === 10 && i > 0 && content[i - 1] === 13) i--;
    }
    if (boundary >= 0) {
      start += boundary;
      content = content.subarray(boundary);
    } else {
      byte_limited = !completeStart || (start > 0 && lines < 200);
      if (!completeStart) {
        let at = content.findIndex((byte) => byte === 10 || byte === 13);
        if (at >= 0) {
          at++;
          if (content[at - 1] === 13 && content[at] === 10) at++;
          if (at < content.length) { start += at; content = content.subarray(at); }
        }
      }
    }
  }
  const next_cursor = start > 0 ? Buffer.from(JSON.stringify([server.name, MOCK_LOG_FILE_IDS.get(node!), bytes.length, node!.mod_time, start])).toString("base64url") : undefined;
  sendJSON(ctx.res, 200, { content: content.toString("base64"), content_sha256: mockSha(content), has_more: start > 0, byte_limited, ...(next_cursor ? { next_cursor } : {}) });
  return true;
}

function parentPath(p: string): string {
  const i = p.lastIndexOf("/");
  return i === -1 ? "" : p.slice(0, i);
}

// cleanFilePath is os.Root's containment in miniature: "." segments and empty
// ones collapse, and ".." or an absolute path is 400 bad_path. null means the
// response has been sent.
function cleanFilePath(ctx: SessionContext, raw: string | null, allowRoot = false): string | null {
  if (raw === null || (raw === "" && !allowRoot)) {
    sendError(ctx.res, 400, "bad_request", "path is required");
    return null;
  }
  if (raw.startsWith("/") || raw.split("/").includes("..") || raw.includes("\0") || Buffer.byteLength(raw) > 4096 || raw.split("/").some((part) => Buffer.byteLength(part) > 255)) {
    sendError(ctx.res, 400, "bad_path", `${raw} escapes the world root`);
    return null;
  }
  const p = raw.split("/").filter((s) => s !== "" && s !== ".").join("/");
  if (p === "" && !allowRoot) {
    sendError(ctx.res, 400, "bad_path", "the world root itself cannot be changed");
    return null;
  }
  return p;
}

function mockSha(data: Buffer): string {
  return createHash("sha256").update(data).digest("hex");
}

async function readRawBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  return Buffer.concat(chunks);
}

function sendMockFileOp(ctx: SessionContext, server: string, kind: FileOp["op"], path: string, error?: FileOp["error"], files = 0, bytes = 0): true {
  const now = new Date().toISOString();
  const op: FileOp = { id: `file-${Date.now()}-${Math.random().toString(36).slice(2)}`, op: kind, path, state: error ? "failed" : "succeeded", started_at: now, finished_at: now, done: bytes, total: bytes, files, bytes, ...(error ? { error } : {}) };
  ctx.state.fileOps[server] = [op, ...(ctx.state.fileOps[server] ?? [])].slice(0, 30);
  sendJSON(ctx.res, 202, { op });
  return true;
}

function landMockFiles(ctx: SessionContext, server: string, kind: FileOp["op"], path: string, tree: MockTree, targets: MockTree, overwrite: boolean, remove: string[] = []): true {
  const conflicts: string[] = [];
  for (const [name, item] of targets) {
    for (let parent = parentPath(name); parent; parent = parentPath(parent)) {
      if (tree.has(parent) && !tree.get(parent)!.is_dir) return sendMockFileOp(ctx, server, kind, path, { code: "type_conflict", message: "parent is a file", entry: parent });
    }
    const existing = tree.get(name);
    if (existing && existing.is_dir !== item.is_dir) return sendMockFileOp(ctx, server, kind, path, { code: "type_conflict", message: "file and folder conflict", entry: name });
    if (existing && !item.is_dir) conflicts.push(name);
  }
  if (conflicts.length && !overwrite) return sendMockFileOp(ctx, server, kind, path, { code: "file_exists", message: "destination files exist", conflicts: conflicts.slice(0, 200), conflict_count: conflicts.length });
  for (const source of remove) for (const key of [...tree.keys()]) if (key === source || key.startsWith(`${source}/`)) tree.delete(key);
  for (const [name, item] of targets) {
    const parts = name.split("/");
    for (let i = 1; i < parts.length; i++) {
      const parent = parts.slice(0, i).join("/");
      if (!tree.has(parent)) tree.set(parent, { is_dir: true, data: Buffer.alloc(0), mod_time: item.mod_time });
    }
    tree.set(name, item);
  }
  const files = [...targets.values()].filter((item) => !item.is_dir);
  return sendMockFileOp(ctx, server, kind, path, undefined, files.length, files.reduce((sum, item) => sum + item.data.length, 0));
}

function publicMockHost(host: string): boolean {
  if (host.startsWith("::ffff:")) {
    const words = host.slice(7).split(":").map((part) => parseInt(part, 16));
    if (words.length === 2) host = [words[0] >> 8, words[0] & 255, words[1] >> 8, words[1] & 255].join(".");
  }
  if (isIP(host) === 6) return /^[23][0-9a-f]{0,3}:/.test(host) && !/^2001:(?:0:|:|db8:)|^2002:/.test(host);
  if (isIP(host) === 0) return !/(^|\.)localhost$/.test(host.replace(/\.$/, ""));
  const [a, b, c] = host.split(".").map(Number);
  return !(a === 0 || a === 10 || a === 127 || a >= 224 || (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) || (a === 192 && (b === 168 || (b === 0 && (c === 0 || c === 2)) || (b === 88 && c === 99))) ||
    (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) || (a === 203 && b === 0 && c === 113));
}

async function handleFileTaskMock(ctx: SessionContext, server: MockServer, tree: MockTree, key: string): Promise<true> {
  const fail = (status: number, code: string, message: string): true => { sendError(ctx.res, status, code, message); return true; };
  const body = await readJSON<{ op?: "copy" | "move"; paths?: string[]; to?: string; url?: string; path?: string; overwrite?: boolean }>(ctx.req);
  const url = new URL(ctx.req.url ?? "/", "http://localhost");
  const now = new Date().toISOString();
  if (key === "POST files/from-url") {
    const path = cleanFilePath(ctx, body.path ?? null);
    if (path === null) return true;
    let source: URL;
    try { source = new URL(body.url ?? ""); } catch { return fail(400, "bad_url", "invalid URL"); }
    const host = source.hostname.replace(/^\[|\]$/g, "").toLowerCase();
    if (!["http:", "https:"].includes(source.protocol) || source.username || source.password || (source.port && Number(source.port) < 1) || !host || !publicMockHost(host) || Buffer.byteLength(body.url ?? "") > 8192) return fail(400, "bad_url", "public HTTP or HTTPS URL required");
    if (!tree.get(parentPath(path))?.is_dir) return fail(404, "not_found", "destination folder not found");
    if (MOCK_MANAGED.has(path)) return fail(400, "bad_path", "managed path");
    return landMockFiles(ctx, server.name, "upload", path, tree, new Map([[path, { is_dir: false, data: Buffer.from(`Mock download from ${source.href}\n`), mod_time: now }]]), body.overwrite === true);
  }
  let paths: string[];
  const batch = Object.hasOwn(body, "paths");
  if (key === "POST files/download" && !batch) {
    const path = cleanFilePath(ctx, url.searchParams.get("path"));
    if (path === null) return true;
    paths = [path];
  } else {
    if (key === "POST files/download" && (url.searchParams.has("path") || url.searchParams.has("dir"))) return fail(400, "bad_request", "selected paths cannot be combined with path or dir");
    if (!Array.isArray(body.paths) || body.paths.length < 1 || body.paths.length > 500 || body.paths.some((p) => typeof p !== "string")) return fail(400, "bad_request", "select 1 to 500 paths");
    paths = [];
    for (const raw of body.paths) { const path = cleanFilePath(ctx, raw); if (path === null) return true; paths.push(path); }
    if (paths.reduce((total, path) => total + Buffer.byteLength(path), 0) > 64 * 1024) return fail(400, "bad_request", "selected paths exceed 65536 bytes");
    const basenames = paths.map((path) => path.split("/").pop());
    if (new Set(basenames).size !== paths.length || paths.some((path) => paths.some((other) => path !== other && path.startsWith(`${other}/`)))) return fail(400, "bad_path", "selected paths must have distinct names without overlapping folders");
  }
  if (paths.some((path) => !tree.has(path))) return fail(404, "not_found", "selected entry not found");
  const selected = [...tree.entries()].filter(([path]) => paths.some((root) => path === root || path.startsWith(`${root}/`)) && path !== MOCK_SECRET_CONFIG);
  const safeData = (path: string, data: Buffer) => path === "server.properties" ? redactMockProperties(data) : data;
  if (key === "POST files/download" || key === "POST files/zip") {
    if (paths.includes(MOCK_SECRET_CONFIG)) return fail(400, "bad_path", "proxy secret cannot be downloaded");
    const data = mockZip(selected.map(([path, node]) => {
      const root = paths.find((name) => path === name || path.startsWith(`${name}/`))!;
      return { path: root.split("/").pop()! + path.slice(root.length), data: safeData(path, node.data), directory: node.is_dir };
    }));
    if (key === "POST files/zip") {
      const to = cleanFilePath(ctx, body.to ?? null);
      if (to === null) return true;
      if (!to.toLowerCase().endsWith(".zip") || paths.some((p) => to === p || to.startsWith(`${p}/`)) || MOCK_MANAGED.has(to)) return fail(400, "bad_path", "invalid archive destination");
      if (!tree.get(parentPath(to))?.is_dir) return fail(404, "not_found", "destination folder not found");
      return landMockFiles(ctx, server.name, "zip", to, tree, new Map([[to, { is_dir: false, data, mod_time: now }]]), body.overwrite === true);
    }
    const one = !batch && url.searchParams.get("dir") !== "true";
    if (!batch && tree.get(paths[0])!.is_dir === one) return fail(400, "bad_path", "file or folder type changed");
    const filename = batch ? `${server.name}-files.zip` : paths[0].split("/").pop()! + (one ? "" : ".zip");
    const ticket = `export-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    ctx.state.fileExports[ticket] = { owner: ctx.account.id, filename, data: one ? safeData(paths[0], tree.get(paths[0])!.data) : data, contentType: one ? "application/octet-stream" : "application/zip", expires: Date.now() + 90_000 };
    sendJSON(ctx.res, 202, { ticket, state: "pending", filename });
    return true;
  }
  if (!["copy", "move"].includes(body.op ?? "")) return fail(400, "bad_request", "copy or move is required");
  const to = cleanFilePath(ctx, body.to ?? null, true);
  if (to === null) return true;
  if (!tree.get(to)?.is_dir) return fail(400, "bad_path", "destination is not a folder");
  if (paths.some((p) => MOCK_MANAGED.has(p) || p === to || to.startsWith(`${p}/`))) return fail(400, "bad_path", "managed or recursive source");
  const targets: MockTree = new Map();
  for (const root of paths) {
    const target = [to, root.split("/").pop()].filter(Boolean).join("/");
    if (target === root) return fail(400, "bad_path", "source and destination match");
    for (const [path, node] of selected) if (path === root || path.startsWith(`${root}/`)) targets.set(target + path.slice(root.length), { ...node, data: safeData(path, node.data), mod_time: now });
  }
  return landMockFiles(ctx, server.name, body.op!, to, tree, targets, body.overwrite === true, body.op === "move" ? paths : []);
}

// File Jobs share a running game's volume; lifecycle transitions and maintenance still gate access.
async function handleFilesMock(ctx: SessionContext, serverInfo: MockServer): Promise<boolean> {
  const route = ctx.parts.slice(4).join("/");
  const known = ["GET files", "GET file", "PUT file", "DELETE file", "POST files/mkdir", "POST files/rename", "PUT files/upload", "GET files/search", "GET files/ops", "POST files/transfer", "POST files/zip", "POST files/from-url", "POST files/unzip", "POST files/download", "GET extensions"];
  const key = `${ctx.method} ${route}`;
  const fail = (status: number, code: string, message: string): true => {
    sendError(ctx.res, status, code, message);
    return true;
  };
  if (!known.includes(key)) return false;
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return true;
  }
  const url = new URL(ctx.req.url ?? "/", "http://localhost");
  const liveConfig = ["login", "lobby"].includes(serverInfo.name) &&
    ["GET file", "PUT file"].includes(key) && url.searchParams.get("path") === "felis-experience.json";
  if (!liveConfig && serverInfo.phase !== "Stopped" && !(serverInfo.phase === "Running" && serverInfo.desiredState === "Running")) {
    sendError(ctx.res, 409, "server_transitioning", "wait for the server to finish starting or stopping before working with its files");
    return true;
  }
  if ((ctx.state.jobs[serverInfo.name] ?? []).some((job) => job.then_restore === "pending" || (job.state === "running" && (job.kind === "backup" || job.kind === "restore")))) {
    sendError(ctx.res, 409, "maintenance_in_progress", "backup or restore holds this server's world");
    return true;
  }
  const tree = filesOf(ctx.state, serverInfo.name);
  if (key === "GET extensions") {
    const entries = [...tree.entries()].filter(([path, node]) => !node.is_dir && ["plugins", "mods"].includes(parentPath(path)) && /\.jar(?:\.disabled)?$/i.test(path))
      .sort(([a], [b]) => Number(a.startsWith("mods/")) - Number(b.startsWith("mods/")) || (a < b ? -1 : a > b ? 1 : 0)).map(([path, node]) => {
        const kind = path.startsWith("plugins/") ? "plugin" : "mod";
        const metadata = node.extension_metadata ?? [];
        const filename = path.split("/").pop()!;
        const base = kind === "mod" ? "config" : "plugins";
        const names = [...metadata.flatMap((item) => [item.id, item.name]), filename.replace(/\.jar(?:\.disabled)?$/i, "")];
        const config = names.filter((name) => name !== "" && name !== "." && name !== ".." && !/[\/\\]/.test(name))
          .map((name) => `${base}/${name}`).find((candidate) => tree.get(candidate)?.is_dir && !/(^|\/)\.felis-(edit|unzip|move|preserved|zip)-[0-9a-fA-F]{12}(\/|$)/.test(candidate))
          ?? (kind === "mod" && tree.get(base)?.is_dir ? base : undefined);
        return { path, filename, size: node.data.length, enabled: !/\.disabled$/i.test(path), kind,
          metadata_status: metadata.length > 0 ? "ok" : "unavailable", metadata,
          ...(config ? { config_path: config } : {}) };
      });
    sendJSON(ctx.res, 200, { entries: entries.slice(0, 500), truncated: entries.length > 500 });
    return true;
  }
  if (key === "GET files/ops") {
    sendJSON(ctx.res, 200, { ops: ctx.state.fileOps[serverInfo.name] ?? [] });
    return true;
  }
  if (["POST files/transfer", "POST files/zip", "POST files/from-url", "POST files/download"].includes(key)) {
    return handleFileTaskMock(ctx, serverInfo, tree, key);
  }
  const p = cleanFilePath(ctx, url.searchParams.get("path"), key === "GET files" || key === "GET files/search");
  if (p === null) return true;
  await new Promise((r) => setTimeout(r, 350));
  const now = new Date().toISOString();
  const node = tree.get(p);
  const parent = tree.get(parentPath(p));

  switch (key) {
    case "GET files/search": {
      const query = url.searchParams.get("q")?.trim();
      if (!query || Buffer.byteLength(query) > 256) return fail(400, "bad_request", "query is required and must not exceed 256 bytes");
      if (!node?.is_dir) return fail(400, "bad_path", "search path is not a directory");
      const prefix = p ? `${p}/` : "";
      const entries = [...tree.entries()].filter(([path]) => {
        if (!path.startsWith(prefix) || path === p) return false;
        const relative = path.slice(prefix.length).split("/");
        return relative.length <= 17 && !relative.slice(0, -1).some((part) => ["region", "entities", "poi"].includes(part)) && relative.at(-1)!.toLowerCase().includes(query.toLowerCase());
      }).map(([path, entry]) => ({ name: path.slice(prefix.length), is_dir: entry.is_dir, size: entry.data.length, mod_time: entry.mod_time }));
      sendJSON(ctx.res, 200, { path: p, entries: entries.slice(0, 500), truncated: entries.length > 500, free_bytes: null });
      return true;
    }
    case "POST files/unzip": {
      if (!node || node.is_dir) return fail(404, "not_found", "archive does not exist");
      const body = await readJSON<{ overwrite?: boolean }>(ctx.req);
      let files;
      try { files = mockArchive(node.data, p); }
      catch (e) {
        const code = e instanceof Error && ["archive_invalid", "archive_symlink", "archive_unsafe"].includes(e.message) ? e.message : "archive_invalid";
        sendMockFileOp(ctx, serverInfo.name, "unzip", p, { code, message: "archive refused" }); return true;
      }
      const root = parentPath(p);
      const targets = new Map<string, MockFileNode>();
      for (const file of files) {
        if (file.path.startsWith("/") || file.path.includes("\\") || file.path.split("/").includes("..")) {
          sendMockFileOp(ctx, serverInfo.name, "unzip", p, { code: "archive_unsafe", message: "unsafe archive entry", entry: file.path }); return true;
        }
        const path = [root, file.path.replace(/\/$/, "")].filter(Boolean).join("/");
        if (MOCK_MANAGED.has(path)) {
          sendMockFileOp(ctx, serverInfo.name, "unzip", p, { code: "bad_path", message: "managed path", entry: file.path }); return true;
        }
        targets.set(path, { is_dir: !!file.directory, data: file.data, mod_time: now });
      }
      return landMockFiles(ctx, serverInfo.name, "unzip", p, tree, targets, body.overwrite === true);
    }
    case "GET files": {
      if (!node) return fail(404, "not_found", `${p} does not exist`);
      if (!node.is_dir) return fail(400, "bad_path", `${p} is not a directory`);
      const entries = [...tree.entries()]
        .filter(([k]) => k !== "" && parentPath(k) === p)
        .map(([k, n]) => ({ name: k.slice(p === "" ? 0 : p.length + 1), size: n.is_dir ? 4096 : n.data.length, is_dir: n.is_dir, mod_time: n.mod_time }))
        .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
      sendJSON(ctx.res, 200, { path: p, entries, truncated: false, free_bytes: null });
      return true;
    }
    case "GET file": {
      if (p === MOCK_SECRET_CONFIG) return fail(400, "bad_path", `${p} holds the proxy forwarding secret`);
      if (!node) return fail(404, "not_found", `${p} does not exist`);
      if (node.is_dir) return fail(400, "bad_path", `${p} is a directory, not a file`);
      if (node.data.length > MOCK_MAX_READ) return fail(413, "too_large", `${p} is larger than the editor reads`);
      const content = p === "server.properties" ? redactMockProperties(node.data) : node.data;
      sendJSON(ctx.res, 200, { path: p, content: content.toString("base64"), sha256: mockSha(node.data), content_sha256: mockSha(content) });
      return true;
    }
    case "PUT file": {
      const body = await readJSON<{ content?: string; expect_sha256?: string; create_only?: boolean }>(ctx.req);
      if (typeof body.content !== "string") return fail(400, "bad_request", "content is required");
      let data: Buffer = Buffer.from(body.content, "base64");
      if (data.length > MOCK_MAX_WRITE) return fail(413, "too_large", "the content is larger than the editor writes");
      if (!parent?.is_dir) return fail(404, "not_found", `folder ${parentPath(p)} does not exist`);
      if (node?.is_dir) return fail(400, "bad_path", `${p} is a directory`);
      if (body.create_only && node) return fail(409, "file_exists", `${p} already exists`);
      if (body.expect_sha256 && (!node || mockSha(node.data) !== body.expect_sha256)) {
        return fail(409, "file_changed", `${p} changed since it was read`);
      }
      if (p === "server.properties" && node) data = preserveMockProperties(data, node.data);
      if (data.length > MOCK_MAX_WRITE) return fail(413, "too_large", "preserving redacted values exceeds the editor limit");
      tree.set(p, { is_dir: false, data, mod_time: now });
      sendJSON(ctx.res, 200, { path: p, status: "written", sha256: mockSha(data) });
      return true;
    }
    case "DELETE file": {
      if (!node) return fail(404, "not_found", `${p} does not exist`);
      for (const k of [...tree.keys()]) if (k === p || k.startsWith(`${p}/`)) tree.delete(k);
      sendJSON(ctx.res, 200, { path: p, status: "deleted" });
      return true;
    }
    case "POST files/mkdir": {
      if (!parent?.is_dir) return fail(404, "not_found", `folder ${parentPath(p)} does not exist`);
      if (node) return fail(409, "file_exists", `${p} already exists`);
      tree.set(p, { is_dir: true, data: Buffer.alloc(0), mod_time: now });
      sendJSON(ctx.res, 200, { path: p, status: "created" });
      return true;
    }
    case "POST files/rename": {
      const body = await readJSON<{ to?: string }>(ctx.req);
      const to = cleanFilePath(ctx, body.to ?? "");
      if (to === null) return true;
      if (!node) return fail(404, "not_found", `${p} does not exist`);
      if (MOCK_MANAGED.has(p)) return fail(400, "bad_path", `${p} is managed by felis and cannot be moved or renamed`);
      if (tree.has(to)) return fail(409, "file_exists", `${to} already exists`);
      if (!tree.get(parentPath(to))?.is_dir) return fail(404, "not_found", `folder ${parentPath(to)} does not exist`);
      for (const [k, n] of [...tree.entries()]) {
        if (k === p || k.startsWith(`${p}/`)) {
          tree.delete(k);
          tree.set(to + k.slice(p.length), n);
        }
      }
      sendJSON(ctx.res, 200, { path: p, to, status: "renamed" });
      return true;
    }
    case "PUT files/upload": {
      const declared = Number(ctx.req.headers["content-length"] ?? NaN);
      if (Number.isNaN(declared)) return fail(411, "length_required", "an upload needs a Content-Length");
      if (declared > MOCK_MAX_UPLOAD) return fail(413, "too_large", "uploads are at most 64 MiB");
      const data = await readRawBody(ctx.req);
      if (!parent?.is_dir) return fail(404, "not_found", `folder ${parentPath(p)} does not exist`);
      if (node?.is_dir) return fail(400, "bad_path", `${p} is a directory`);
      if (node && url.searchParams.get("overwrite") !== "true") return fail(409, "file_exists", `${p} already exists`);
      // Landing takes the Job a moment after the body is in.
      await new Promise((r) => setTimeout(r, 900));
      tree.set(p, { is_dir: false, data, mod_time: now });
      sendJSON(ctx.res, 200, { path: p, status: "uploaded", sha256: mockSha(data), size: data.length });
      return true;
    }
  }
  return false;
}

// Match backend authorization order: role, backup lookup, former owner, then stopped-state checks.
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

  // Non-admins may restore only a world they once owned.
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

// Match owner/admin and lifecycle admission; mock backup Jobs finish immediately.
function handleBackupNowMock(ctx: SessionContext, serverInfo: MockServer): void {
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return;
  }
  if (serverInfo.phase !== "Stopped" && serverInfo.phase !== "Running") {
    sendError(ctx.res, 409, "not_stopped", "the server is starting or stopping; back it up once it is running or stopped");
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

// Preserve vanilla's " was banned by " delimiter, including colons and spaces in reasons.
function banlistOutput(banned: string[]): string {
  if (banned.length === 0) return "There are no bans.";
  const head = `There are ${banned.length} ban(s):`;
  const lines = banned.map((p) => `${p} was banned by Server: Banned by an operator.`);
  return [head, ...lines].join("\n");
}

// Whitelist reads and writes both require owner/admin access and a running server.
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
  let body: { command?: unknown };
  try { body = await readJSON(ctx.req); }
  catch { sendError(ctx.res, 400, "bad_request", "invalid command request"); return; }
  const cmd = typeof body?.command === "string" ? body.command.trim().replace(/^\//, "").trim() : "";
  if (!cmd || Object.keys(body).some((key) => key !== "command") || Buffer.byteLength(cmd) > 1024 || /[\x00-\x1f]/.test(cmd)) {
    sendError(ctx.res, 400, "bad_request", "command must be a single line of at most 1024 bytes");
    return;
  }
  if (serverInfo.desiredState !== "Running" || !["Starting", "Running"].includes(serverInfo.phase)) {
    sendError(
      ctx.res,
      409,
      "not_running",
      "server is not running; wake it before sending console commands",
    );
    return;
  }
  if ((ctx.state.jobs[serverInfo.name] ?? []).some((job) => job.then_restore === "pending" || (job.state === "running" && ["backup", "restore", "migration", "reap"].includes(job.kind)))) {
    sendError(ctx.res, 409, "maintenance_in_progress", "another operation holds this server's world");
    return;
  }
  const transport = serverInfo.phase === "Running" && serverInfo.ready ? "rcon" : "stdin";
  sendJSON(ctx.res, 200, { name: serverInfo.name, output: transport === "rcon" ? mockCommandReply(cmd) : "", transport });
}

function mockCommandReply(cmd: string): string {
  const lower = cmd.toLowerCase();
  if (lower === "list") return "There are 2 of a max 20 players online: mock_player, test_player";
  if (lower === "tps" || lower === "forge tps") return "TPS from last 5s, 10s, 1m, 5m, 15m: 20.00, 20.00, *19.87, 19.95, 19.98";
  if (cmd.startsWith("say ")) return `[mock_server] ${cmd.slice(4)}`;
  if (lower === "help") return "--- Showing help ---\n/felis\n/msg\n/list\n/rules";
  return `[mock] command "${cmd}" executed`;
}

// Without a reaper, mock retirement remains pending until cancelled.
async function handleRetirementMock(ctx: SessionContext, serverInfo: MockServer): Promise<void> {
  if (!canManage(ctx.account, serverInfo)) {
    sendError(ctx.res, 403, "forbidden", "server is not owned by this account");
    return;
  }
  const admin = isAdmin(ctx.account.role);
  if (is("DELETE", ctx)) {
    if (serverInfo.retiring?.delete && !admin) {
      sendError(ctx.res, 403, "forbidden", "only an administrator can cancel the deletion of a server");
      return;
    }
    serverInfo.retiring = undefined;
    ctx.res.statusCode = 204;
    ctx.res.end();
    return;
  }
  const body = await readJSON<{ confirm?: string; delete?: boolean }>(ctx.req);
  if (body.delete && !admin) {
    sendError(ctx.res, 403, "forbidden", "only an administrator can delete a server");
    return;
  }
  if (body.confirm !== serverInfo.name) {
    sendError(ctx.res, 400, "confirm_mismatch", "type the server's name to confirm");
    return;
  }
  if (serverInfo.reaperExempt) {
    sendError(ctx.res, 409, "system_server", "a system server cannot be given up or deleted");
    return;
  }
  setPhase(serverInfo, "Stopped");
  serverInfo.retiring = {
    requested_at: serverInfo.retiring?.requested_at ?? new Date().toISOString(),
    delete: (serverInfo.retiring?.delete ?? false) || body.delete === true,
  };
  sendJSON(ctx.res, 202, { name: serverInfo.name, retiring: serverInfo.retiring });
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
