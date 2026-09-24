import { describe, it, expect } from "vitest";
import {
  LogStreamController,
  classifyLogLine,
  type EventSourceFactory,
  type EventSourceLike,
} from "./logstream";

// FakeEventSource drives the controller without a browser: Node has no
// EventSource, and even in jsdom it would dial a real socket. The controller
// takes its EventSource via a factory precisely so this fake can model the
// lifecycle — open, message, transient drop (reconnecting), fatal close (ended),
// and teardown — deterministically. It matches EventSourceLike structurally.
class FakeEventSource implements EventSourceLike {
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  readyState = 0; // CONNECTING
  closed = false;
  readonly url: string;
  private readonly listeners = new Map<string, Array<(ev: unknown) => void>>();

  constructor(url: string) {
    this.url = url;
  }

  close(): void {
    this.closed = true;
    this.readyState = 2; // CLOSED
  }

  addEventListener(type: string, listener: (ev: unknown) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  emitEvent(type: string): void {
    for (const fn of this.listeners.get(type) ?? []) fn({ data: "" });
  }

  // Test drivers mirroring what the browser would invoke.
  emitOpen(): void {
    this.readyState = 1; // OPEN
    this.onopen?.({});
  }
  emitMessage(data: string): void {
    this.onmessage?.({ data });
  }
  emitError(readyState: number): void {
    this.readyState = readyState;
    this.onerror?.({});
  }
}

function makeFactory(): { factory: EventSourceFactory; created: FakeEventSource[] } {
  const created: FakeEventSource[] = [];
  const factory: EventSourceFactory = (url) => {
    const es = new FakeEventSource(url);
    created.push(es);
    return es;
  };
  return { factory, created };
}

describe("classifyLogLine", () => {
  const cases: Array<[string, ReturnType<typeof classifyLogLine>]> = [
    ["[12:30:01] [Server thread/INFO]: Done (5.123s)!", "info"],
    ["[12:30:02] [Server thread/WARN]: Can't keep up!", "warn"],
    ["[12:30:03] [Server thread/ERROR]: Encountered an exception", "error"],
    ["[12:30:04] [Server thread/SEVERE]: fatal boot failure", "error"],
    ["[12:30:05] [Worker/FATAL]: out of memory", "error"],
    ["[12:30:06] [Server thread/DEBUG]: tick took 1ms", "debug"],
    ["[12:30:07] [Server thread/TRACE]: packet in", "debug"],
    ["[12:30:08] [Server thread/WARNING]: deprecated config", "warn"],
    ["[INFO] bracketed level form", "info"],
    ["    at net.minecraft.server.Foo.bar(Foo.java:42)", "plain"],
    ["<Steve> the build had an ERROR earlier lol", "plain"], // chat must not tint
    ["plain server line with no marker", "plain"],
  ];
  for (const [line, want] of cases) {
    it(`classifies ${JSON.stringify(line)} as ${want}`, () => {
      expect(classifyLogLine(line)).toBe(want);
    });
  }
});

describe("LogStreamController", () => {
  it("transitions connecting -> open and appends classified lines", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/api/v1/servers/s/console", factory });
    ctrl.open();
    expect(created).toHaveLength(1);
    expect(created[0].url).toBe("/api/v1/servers/s/console");
    expect(ctrl.getSnapshot().status).toBe("connecting");

    created[0].emitOpen();
    expect(ctrl.getSnapshot().status).toBe("open");

    created[0].emitMessage("[12:00:00] [Server thread/WARN]: heads up");
    const { lines } = ctrl.getSnapshot();
    expect(lines).toHaveLength(1);
    expect(lines[0]).toMatchObject({ seq: 0, level: "warn" });
    expect(lines[0].text).toContain("heads up");
  });

  it("bounds the ring buffer, dropping the oldest lines", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, maxLines: 2 });
    ctrl.open();
    created[0].emitMessage("line0");
    created[0].emitMessage("line1");
    created[0].emitMessage("line2");
    const { lines } = ctrl.getSnapshot();
    expect(lines).toHaveLength(2);
    // Oldest dropped; the survivors keep their monotonic seq (1 then 2).
    expect(lines.map((l) => l.text)).toEqual(["line1", "line2"]);
    expect(lines.map((l) => l.seq)).toEqual([1, 2]);
  });

  it("returns a stable snapshot reference between mutations", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open(); // status already 'connecting' -> no commit, snapshot stable
    const s1 = ctrl.getSnapshot();
    expect(ctrl.getSnapshot()).toBe(s1);
    created[0].emitMessage("x");
    expect(ctrl.getSnapshot()).not.toBe(s1);
  });

  it("notifies subscribers on mutation and stops after unsubscribe", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open();
    let hits = 0;
    const unsub = ctrl.subscribe(() => {
      hits++;
    });
    created[0].emitOpen(); // commit
    created[0].emitMessage("a"); // commit
    expect(hits).toBe(2);
    unsub();
    created[0].emitMessage("b");
    expect(hits).toBe(2); // no further notifications
  });

  it("maps a transient drop to reconnecting and a fatal close to ended", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open();
    created[0].emitOpen();
    created[0].emitError(0); // readyState CONNECTING -> auto-retrying
    expect(ctrl.getSnapshot().status).toBe("reconnecting");
    created[0].emitError(2); // readyState CLOSED -> fatal (e.g. 409 not_running)
    expect(ctrl.getSnapshot().status).toBe("ended");
  });

  it("ignores events after close (teardown linchpin)", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open();
    created[0].emitOpen();
    ctrl.close();
    expect(created[0].closed).toBe(true);
    // A late event from the now-detached source must not mutate state.
    created[0].emitMessage("ghost");
    created[0].emitError(0);
    expect(ctrl.getSnapshot().lines).toHaveLength(0);
    expect(ctrl.getSnapshot().status).toBe("open");
  });

  it("reconnect closes the old stream and dials a fresh one", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open();
    created[0].emitError(2); // ended
    ctrl.reconnect();
    expect(created).toHaveLength(2);
    expect(created[0].closed).toBe(true);
    expect(ctrl.getSnapshot().status).toBe("connecting");
    created[1].emitOpen();
    expect(ctrl.getSnapshot().status).toBe("open");
  });

  it("clear empties the buffer without disturbing status", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory });
    ctrl.open();
    created[0].emitOpen();
    created[0].emitMessage("a");
    ctrl.clear();
    expect(ctrl.getSnapshot().lines).toHaveLength(0);
    expect(ctrl.getSnapshot().status).toBe("open");
  });

  it("ends for good when the server revokes the stream", () => {
    const { factory, created } = makeFactory();
    const ctrl = new LogStreamController({ url: "/api/v1/servers/s/console", factory });
    ctrl.open();
    const es = created[0];
    es.emitOpen();
    es.emitMessage("[12:00:00] [Server thread/INFO]: hello");
    es.emitEvent("revoked");
    expect(ctrl.getSnapshot().status).toBe("ended");
    expect(es.closed).toBe(true);
    expect(ctrl.getSnapshot().lines).toHaveLength(1);
    expect(created).toHaveLength(1);
  });
});
