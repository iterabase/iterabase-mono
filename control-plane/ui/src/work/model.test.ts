import { describe, expect, it } from "vitest";
import * as fx from "../test/fixtures";
import {
  coworkerState,
  decisionsOf,
  groupFailures,
  groupItems,
  groupOf,
  hoursAndMinutes,
  nextDeadline,
  periodFor,
} from "./model";

describe("work model", () => {
  it("never puts waiting or paused work in Needs you", () => {
    expect(groupOf("waiting")).toBe("waiting");
    expect(groupOf("paused")).toBe("waiting");
    expect(groupOf("blocked")).toBe("needs");
    expect(groupOf("todo")).toBe("working");
    const groups = groupItems(fx.items);
    expect(groups.needs.map((item) => item.id)).toEqual([
      fx.login.id,
      fx.question.id,
      fx.approval.id,
    ]);
    expect(groups.waiting.map((item) => item.id)).toEqual([
      fx.waiting.id,
      fx.paused.id,
    ]);
    expect(groups.failed.map((item) => item.id)).toEqual([fx.failed.id]);
  });

  it("orders decisions oldest first", () => {
    expect(decisionsOf(fx.items).map((item) => item.id)).toEqual([
      fx.login.id,
      fx.question.id,
      fx.approval.id,
    ]);
  });

  it("groups failures by reason", () => {
    const other = {
      ...fx.failed,
      id: "w-x",
      failureSummary: { code: "unheard_of" },
    };
    expect(
      groupFailures([fx.failed, other, fx.failed]).map((g) => [
        g.code,
        g.items.length,
      ]),
    ).toEqual([
      ["execution_failed", 2],
      ["other", 1],
    ]);
  });

  it("derives a coworker's live state from its work", () => {
    expect(coworkerState([fx.working, fx.failed])).toBe("working");
    expect(coworkerState([fx.failed])).toBe("failed");
    expect(coworkerState([fx.waiting])).toBe("waiting");
    expect(coworkerState([fx.done])).toBe("available");
  });

  it("only names a next deadline that is still ahead", () => {
    expect(nextDeadline(fx.items)?.toISOString()).toBe(
      fx.waiting.wait!.deadlineAt,
    );
    expect(
      nextDeadline([fx.waiting], new Date(Date.now() + 24 * 3600_000)),
    ).toBeUndefined();
  });

  it("builds calendar periods and hour estimates", () => {
    const wednesday = new Date(2026, 9, 14, 10, 30);
    expect(periodFor("week", wednesday).from).toEqual(new Date(2026, 9, 12));
    expect(periodFor("month", wednesday).from).toEqual(new Date(2026, 9, 1));
    expect(periodFor("today", wednesday).to).toEqual(new Date(2026, 9, 15));
    expect(hoursAndMinutes(6600)).toEqual({ hours: 1, minutes: 50 });
  });
});
