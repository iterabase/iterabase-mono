import { describe, expect, it } from "vitest";
import { parseFrame, reconnectDelay } from "./events";

describe("work event stream", () => {
  it("reads the cursor, code and item from a frame", () => {
    expect(
      parseFrame(
        'id: 42\nevent: work_blocked\ndata: {"workItemId":"w-1","code":"work_blocked"}',
      ),
    ).toEqual({ cursor: "42", code: "work_blocked", workItemId: "w-1" });
  });

  it("ignores heartbeats and keeps the cursor of malformed data", () => {
    expect(parseFrame(": heartbeat")).toBeNull();
    expect(parseFrame("id: 7\nevent: x\ndata: {")).toEqual({
      cursor: "7",
      code: "x",
    });
  });

  it("backs off reconnects up to 15 seconds", () => {
    expect([1, 2, 3, 4, 5, 9].map(reconnectDelay)).toEqual([
      1000, 2000, 4000, 8000, 15000, 15000,
    ]);
  });
});
