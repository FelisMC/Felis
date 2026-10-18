// Use credentialed EventSource for authenticated console SSE; it cannot set Authorization headers.

import { parseFormatting, type Segment } from "./mcformat";

/** LogLevel is the coarse severity used only for tinting a line. */
export type LogLevel = "error" | "warn" | "info" | "debug" | "plain";

// Native transient retries use reconnecting; fatal closure requires manual reconnection.
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

// EventSource readyState constants, defined locally so the module loads under Node, where the DOM EventSource is undefined.
const ES_CLOSED = 2;

// Handler parameters must remain compatible with the DOM EventSource interface.
// Only .data is read, so the looseness costs nothing.
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

// Read levels from log4j's thread/LEVEL marker.
// Do not infer severity from chat text; unrecognized continuations remain plain.
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

// Bound buffering and frame notifications; close streams during effect cleanup to prevent duplicate mounts leaking connections.
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
    // Treat the server's revoked event as final instead of retrying an expired or unauthorized stream.
    es.addEventListener("revoked", () => {
      if (this.es !== es) return;
      this.disconnect();
      this.retryable = false;
      this.setStatus("ended");
    });
    es.onerror = () => {
      // Native EventSource retries transient drops but closes permanently on invalid status or content type.
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

  // Clear any scheduled frame when flushing pending lines, including flushes triggered by status changes.
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
