import { describe, it, expect } from "vitest";
import {
  LogStreamController,
  classifyLogLine,
  type EventSourceFactory,
  type EventSourceLike,
  type FrameScheduler,
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

// ManualFrames stands in for requestAnimationFrame: queued callbacks run only
// when the test says a frame has passed.
class ManualFrames {
  private queue: Array<() => void> = [];
  schedule: FrameScheduler = (cb) => {
    this.queue.push(cb);
    return () => {
      this.queue = this.queue.filter((f) => f !== cb);
    };
  };
  get queued(): number {
    return this.queue.length;
  }
  run(): void {
    const due = this.queue;
    this.queue = [];
    due.forEach((f) => f());
  }
}

function makeFactory(): { factory: EventSourceFactory; created: FakeEventSource[]; frames: ManualFrames } {
  const created: FakeEventSource[] = [];
  const factory: EventSourceFactory = (url) => {
    const es = new FakeEventSource(url);
    created.push(es);
    return es;
  };
  return { factory, created, frames: new ManualFrames() };
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
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/api/v1/servers/s/console", factory, frame: frames.schedule });
    ctrl.open();
    expect(created).toHaveLength(1);
    expect(created[0].url).toBe("/api/v1/servers/s/console");
    expect(ctrl.getSnapshot().status).toBe("connecting");

    created[0].emitOpen();
    expect(ctrl.getSnapshot().status).toBe("open");

    created[0].emitMessage("[12:00:00] [Server thread/WARN]: heads up");
    frames.run();
    const { lines } = ctrl.getSnapshot();
    expect(lines).toHaveLength(1);
    expect(lines[0]).toMatchObject({ seq: 0, level: "warn" });
    expect(lines[0].text).toContain("heads up");
  });

  it("bounds the ring buffer, dropping the oldest lines", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, maxLines: 2, frame: frames.schedule });
    ctrl.open();
    created[0].emitMessage("line0");
    created[0].emitMessage("line1");
    created[0].emitMessage("line2");
    frames.run();
    const { lines } = ctrl.getSnapshot();
    expect(lines).toHaveLength(2);
    // Oldest dropped; the survivors keep their monotonic seq (1 then 2).
    expect(lines.map((l) => l.text)).toEqual(["line1", "line2"]);
    expect(lines.map((l) => l.seq)).toEqual([1, 2]);
  });

  it("returns a stable snapshot reference between mutations", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open(); // status already 'connecting' -> no commit, snapshot stable
    const s1 = ctrl.getSnapshot();
    expect(ctrl.getSnapshot()).toBe(s1);
    created[0].emitMessage("x");
    expect(ctrl.getSnapshot()).toBe(s1); // queued until the frame
    frames.run();
    expect(ctrl.getSnapshot()).not.toBe(s1);
  });

  it("notifies subscribers on mutation and stops after unsubscribe", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    let hits = 0;
    const unsub = ctrl.subscribe(() => {
      hits++;
    });
    created[0].emitOpen(); // commit
    created[0].emitMessage("a");
    frames.run(); // commit
    expect(hits).toBe(2);
    unsub();
    created[0].emitMessage("b");
    frames.run();
    expect(hits).toBe(2); // no further notifications
  });

  it("maps a transient drop to reconnecting and a fatal close to ended", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitOpen();
    created[0].emitError(0); // readyState CONNECTING -> auto-retrying
    expect(ctrl.getSnapshot().status).toBe("reconnecting");
    created[0].emitError(2); // readyState CLOSED -> fatal (e.g. 409 not_running)
    expect(ctrl.getSnapshot().status).toBe("ended");
  });

  it("ignores events after close (teardown linchpin)", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
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
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
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
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitOpen();
    created[0].emitMessage("a");
    frames.run();
    created[0].emitMessage("b"); // still queued
    ctrl.clear();
    frames.run();
    expect(ctrl.getSnapshot().lines).toHaveLength(0);
    expect(ctrl.getSnapshot().status).toBe("open");
  });

  it("ends for good when the server revokes the stream", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/api/v1/servers/s/console", factory, frame: frames.schedule });
    ctrl.open();
    const es = created[0];
    es.emitOpen();
    es.emitMessage("[12:00:00] [Server thread/INFO]: hello");
    es.emitEvent("revoked"); // no frame ran: the status change carries the line
    expect(ctrl.getSnapshot().status).toBe("ended");
    expect(es.closed).toBe(true);
    expect(ctrl.getSnapshot().lines).toHaveLength(1);
    expect(created).toHaveLength(1);
  });
  it("delivers a burst of lines in one commit per frame", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitOpen();
    let hits = 0;
    ctrl.subscribe(() => hits++);

    for (let i = 0; i < 500; i++) created[0].emitMessage(`mod ${i}`);
    expect(hits).toBe(0);
    expect(frames.queued).toBe(1);

    frames.run();
    expect(hits).toBe(1);
    expect(ctrl.getSnapshot().lines.map((l) => l.text)).toEqual(
      Array.from({ length: 500 }, (_, i) => `mod ${i}`),
    );

    created[0].emitMessage("next");
    expect(frames.queued).toBe(1);
    frames.run();
    expect(hits).toBe(2);
    expect(ctrl.getSnapshot().lines.at(-1)?.seq).toBe(500);
  });

  it("keeps the queue bounded while no frame runs, as in a background tab", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, maxLines: 3, frame: frames.schedule });
    ctrl.open();
    for (let i = 0; i < 100; i++) created[0].emitMessage(`line${i}`);
    expect((ctrl as unknown as { pending: unknown[] }).pending.length).toBeLessThan(6);

    frames.run();
    expect(ctrl.getSnapshot().lines.map((l) => l.text)).toEqual(["line97", "line98", "line99"]);
  });

  it("shows queued lines together with a status change", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitOpen();
    let hits = 0;
    ctrl.subscribe(() => hits++);
    created[0].emitMessage("Stopping server");
    created[0].emitMessage("Saving chunks");
    created[0].emitError(2);

    expect(hits).toBe(1);
    expect(ctrl.getSnapshot().status).toBe("ended");
    expect(ctrl.getSnapshot().lines.map((l) => l.text)).toEqual(["Stopping server", "Saving chunks"]);
    expect(frames.queued).toBe(0);
  });

  it("drops the queued frame on close", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitMessage("late");
    ctrl.close();
    expect(frames.queued).toBe(0);
    frames.run();
    expect(ctrl.getSnapshot().lines).toHaveLength(0);
  });

  it("strips colour codes before classifying and keeps them as segments", () => {
    const { factory, created, frames } = makeFactory();
    const ctrl = new LogStreamController({ url: "/c", factory, frame: frames.schedule });
    ctrl.open();
    created[0].emitMessage("[12:00:00] [Server thread/§cERROR§r]: §eplugin§r failed");
    created[0].emitMessage("[12:00:01] [Server thread/INFO]: plain");
    frames.run();

    const [colored, plain] = ctrl.getSnapshot().lines;
    expect(colored.text).toBe("[12:00:00] [Server thread/ERROR]: plugin failed");
    expect(colored.level).toBe("error");
    expect(colored.segments?.find((s) => s.text === "plugin")?.color).toBe("#ffff55");
    expect(plain.segments).toBeUndefined();
  });
});
