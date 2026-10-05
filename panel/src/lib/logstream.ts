// Read-side console core (spec §8 读=pods/log follow). The felis-api endpoint
// `GET /servers/{name}/console` relays the running pod's log as Server-Sent
// Events — one `data: <line>` per log line — authenticated by the same
// Cloudflare Access cookie the rest of api.ts rides on (credentials:"include").
// A native browser EventSource with { withCredentials: true } therefore attaches
// directly; no Authorization header is needed (EventSource cannot set one).
//
// This module is deliberately framework-agnostic: the EventSource is injected
// (see EventSourceFactory) so the lifecycle — status transitions, the bounded
// ring buffer, frame batching, reconnect, teardown — is unit-tested under Node
// against a fake, not left to a "tsc + build" green that only proves it
// compiles. The thin React binding lives in useLogStream.ts; the viewport in
// components/LogConsole.tsx.

import { parseFormatting, type Segment } from "./mcformat";

/** LogLevel is the coarse severity used only for tinting a line. */
export type LogLevel = "error" | "warn" | "info" | "debug" | "plain";

/**
 * StreamStatus models the EventSource lifecycle as the UI cares about it:
 *  - connecting:   the initial dial (or a manual reconnect) is in flight
 *  - open:         the stream is live and delivering lines
 *  - reconnecting: a transient drop; native EventSource is auto-retrying
 *                  (readyState returned to CONNECTING after onerror)
 *  - ended:        a fatal close — a non-200 response (e.g. the 409 a stopped
 *                  server returns, or wrong content-type) makes EventSource go
 *                  CLOSED with no auto-retry; the user may reconnect manually
 */
export type StreamStatus = "connecting" | "open" | "reconnecting" | "ended";

/** A single rendered log line. seq is a monotonic id for stable React keys. */
export interface LogLine {
  seq: number;
  /** The line without § or ANSI codes. */
  text: string;
  level: LogLevel;
  /** Styled runs when the line carried colour codes (see mcformat.ts). */
  segments?: Segment[];
}

/** Immutable snapshot consumed by useSyncExternalStore. */
export interface LogStreamSnapshot {
  lines: LogLine[];
  status: StreamStatus;
  retryable: boolean;
}

// EventSource readyState constants. We avoid referencing the DOM EventSource
// global here so the module loads under Node (tests) where it is undefined.
const ES_CLOSED = 2;

/**
 * EventSourceLike is the minimal slice of the DOM EventSource the controller
 * depends on, so a fake can drive it in tests. The handler params are `any`
 * rather than a precise event type on purpose: under strictFunctionTypes a real
 * browser EventSource (handlers typed `(ev: Event)` / `(ev: MessageEvent)`) is
 * only assignable to this shim if the shim's params are at least as wide as the
 * DOM's — `any` is bivariant, so a browser EventSource satisfies it structurally
 * while a stricter test fake (e.g. `(ev: { data: string })`) still implements
 * it. We never inspect the event beyond `.data`, so the looseness costs nothing.
 */
export interface EventSourceLike {
  onopen: ((ev: any) => void) | null;
  onmessage: ((ev: any) => void) | null;
  onerror: ((ev: any) => void) | null;
  readyState: number;
  close(): void;
  addEventListener(type: string, listener: (ev: any) => void): void;
}

/** Builds an EventSource for a URL (production: the browser ctor; tests: a fake). */
export type EventSourceFactory = (url: string) => EventSourceLike;

/** Runs cb once before the next paint and returns a cancel (tests drive it by hand). */
export type FrameScheduler = (cb: () => void) => () => void;

const nextFrame: FrameScheduler = (cb) => {
  if (typeof requestAnimationFrame === "function") {
    const id = requestAnimationFrame(cb);
    return () => cancelAnimationFrame(id);
  }
  const id = setTimeout(cb, 16);
  return () => clearTimeout(id);
};

export interface LogStreamOptions {
  url: string;
  factory: EventSourceFactory;
  /** Ring-buffer bound; older lines are dropped past this (default 2000). */
  maxLines?: number;
  /** Defers a batch commit; requestAnimationFrame in the browser. */
  frame?: FrameScheduler;
}

const DEFAULT_MAX_LINES = 2000;

/**
 * classifyLogLine maps a Minecraft log line to a coarse level for tinting.
 *
 * Minecraft and its proxies/loaders (vanilla, Paper, Fabric, Forge, NeoForge,
 * Velocity) all log via log4j2, whose default pattern carries the level as a
 * `thread/LEVEL]` marker, e.g.
 *
 *   [12:30:01] [Server thread/INFO]: Done (5.123s)! For help, type "help"
 *   [12:30:02] [Server thread/WARN]: Can't keep up! Is the server overloaded?
 *   [12:30:03] [Server thread/ERROR]: Encountered an unexpected exception
 *
 * We match that marker (and the bracketed `[LEVEL]` variant) rather than scanning
 * the whole line, so a chat message that merely mentions "error" is not tinted
 * red. Anything unrecognized — stack-trace continuations, plain output — is
 * "plain" and renders in the default colour.
 */
export function classifyLogLine(line: string): LogLevel {
  const m = line.match(/(?:[/[]|^\[\d{2}:\d{2}:\d{2} )(SEVERE|FATAL|ERROR|WARNING|WARN|INFO|DEBUG|TRACE)\]/);
  if (!m) return "plain";
  switch (m[1]) {
    case "SEVERE":
    case "FATAL":
    case "ERROR":
      return "error";
    case "WARNING":
    case "WARN":
      return "warn";
    case "INFO":
      return "info";
    default:
      return "debug"; // DEBUG, TRACE
  }
}

/**
 * LogStreamController owns one EventSource and exposes a useSyncExternalStore
 * surface (subscribe + cached getSnapshot). It is the single place that handles:
 *
 *  - status transitions driven by EventSource events,
 *  - the bounded ring buffer (memory stays capped under a long live follow),
 *  - frame batching: a modpack boot logs hundreds of lines a second, so lines
 *    queue up and reach subscribers at most once per frame, one copy of the
 *    buffer per frame instead of one per line,
 *  - reconnect (manual, after a fatal `ended`),
 *  - teardown (close()) — the linchpin that the React effect's cleanup calls so
 *    a StrictMode dev double-mount, or a route change, never leaks a stream.
 */
export class LogStreamController {
  private es: EventSourceLike | null = null;
  private lines: LogLine[] = [];
  // Lines that arrived since the last frame.
  private pending: LogLine[] = [];
  private cancelFrame: (() => void) | null = null;
  private status: StreamStatus = "connecting";
  private seq = 0;
  private readonly url: string;
  private readonly factory: EventSourceFactory;
  private readonly maxLines: number;
  private readonly frame: FrameScheduler;
  private readonly listeners = new Set<() => void>();
  // snapshot is rebuilt only inside commit(), so getSnapshot returns a stable
  // reference between mutations — required by useSyncExternalStore to avoid an
  // infinite render loop.
  private snapshot: LogStreamSnapshot;
  private retryable = true;

  constructor(opts: LogStreamOptions) {
    this.url = opts.url;
    this.factory = opts.factory;
    this.maxLines = opts.maxLines ?? DEFAULT_MAX_LINES;
    this.frame = opts.frame ?? nextFrame;
    this.snapshot = { lines: this.lines, status: this.status, retryable: this.retryable };
  }

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  };

  getSnapshot = (): LogStreamSnapshot => this.snapshot;

  /** open dials the stream. Call once after construction (the hook's effect). */
  open(): void {
    this.connect();
  }

  /** reconnect closes any current stream and dials again (manual retry). */
  reconnect(): void {
    this.disconnect();
    this.connect();
  }

  /** clear empties the buffer without touching the stream. */
  clear(): void {
    this.dropPending();
    this.lines = [];
    this.commit();
  }

  /** close tears the stream down for good. Idempotent. */
  close(): void {
    this.dropPending();
    this.disconnect();
  }

  private connect(): void {
    this.retryable = true;
    this.setStatus("connecting");
    const es = this.factory(this.url);
    this.es = es;
    es.onopen = () => this.setStatus("open");
    es.onmessage = (ev) => this.pushLine(ev.data);
    // The server re-checks a running stream's grant (session still live, caller
    // still owns the server) and sends "revoked" before it closes. That close is
    // final: an auto-retry would only be refused again.
    es.addEventListener("revoked", () => {
      if (this.es !== es) return;
      this.disconnect();
      this.retryable = false;
      this.setStatus("ended");
    });
    es.onerror = () => {
      // Native EventSource auto-reconnects on a transient drop (readyState
      // returns to CONNECTING); only a fatal response — non-200 / wrong
      // content-type, e.g. the 409 a stopped server returns — goes CLOSED, with
      // no further retry. Distinguish so the UI shows "reconnecting" vs "ended".
      if (this.es && this.es.readyState === ES_CLOSED) {
        this.setStatus("ended");
      } else {
        this.setStatus("reconnecting");
      }
    };
  }

  private disconnect(): void {
    if (this.es) {
      // Detach handlers first so a close()-induced onerror can't flip status.
      this.es.onopen = null;
      this.es.onmessage = null;
      this.es.onerror = null;
      this.es.close();
      this.es = null;
    }
  }

  private pushLine(raw: string): void {
    const { text, segments } = parseFormatting(raw);
    const line: LogLine = { seq: this.seq++, text, level: classifyLogLine(text) };
    if (segments) line.segments = segments;
    this.pending.push(line);
    // A background tab gets no frames, so the queue is bounded too. It trims
    // only once it holds twice the buffer, keeping each push O(1) on average.
    if (this.pending.length >= this.maxLines * 2) {
      this.pending = this.pending.slice(-this.maxLines);
    }
    this.cancelFrame ??= this.frame(() => {
      if (this.flushPending()) this.commit();
    });
  }

  // flushPending moves the queued lines into the buffer and reports whether
  // there were any. It also forgets the scheduled frame, whether it is the one
  // running now or one a status change got ahead of.
  private flushPending(): boolean {
    this.cancelFrame?.();
    this.cancelFrame = null;
    if (this.pending.length === 0) return false;
    const next = this.lines.concat(this.pending);
    this.pending = [];
    this.lines = next.length > this.maxLines ? next.slice(-this.maxLines) : next;
    return true;
  }

  private dropPending(): void {
    this.cancelFrame?.();
    this.cancelFrame = null;
    this.pending = [];
  }

  private setStatus(s: StreamStatus): void {
    if (this.status === s) return;
    // Lines that came before the change are shown with it, never after.
    this.flushPending();
    this.status = s;
    this.commit();
  }

  private commit(): void {
    this.snapshot = { lines: this.lines, status: this.status, retryable: this.retryable };
    this.listeners.forEach((fn) => fn());
  }
}
