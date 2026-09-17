import { describe, expect, it } from "vitest";

import { pollInterval } from "@/api/queries";
import type { Series } from "@/composables/series";
import { ago, broken, extent, marks } from "@/lib/chart";

function line(id: string, ...instants: number[]): Series {
  return {
    id,
    index: 0,
    label: id,
    points: instants.map((x) => ({ x, y: 1 })),
  };
}

describe("broken", () => {
  it("joins readings a poll apart", () => {
    const points = [
      { x: 0, y: 1 },
      { x: pollInterval, y: 2 },
      { x: pollInterval * 2, y: 3 },
    ];

    expect(broken(points)).toEqual(points);
  });

  it("breaks the line where polling paused", () => {
    const points = [
      { x: 0, y: 1 },
      { x: pollInterval * 10, y: 2 },
    ];

    expect(broken(points)).toEqual([
      { x: 0, y: 1 },
      { x: pollInterval * 1.5, y: null },
      { x: pollInterval * 10, y: 2 },
    ]);
  });

  it("allows a couple of missed polls before breaking", () => {
    const points = [
      { x: 0, y: 1 },
      { x: pollInterval * 3, y: 2 },
    ];

    expect(broken(points)).toEqual(points);
  });

  it("draws nothing from nothing", () => {
    expect(broken([])).toEqual([]);
  });
});

describe("extent", () => {
  it("spans the earliest and latest reading of any line", () => {
    expect(extent([line("a", 20, 30), line("b", 10, 25)])).toEqual([10, 30]);
  });

  it("reports nothing when no line has a reading", () => {
    expect(extent([])).toBeNull();
    expect(extent([line("a")])).toBeNull();
  });
});

describe("ago", () => {
  it.each([
    [0, "now"],
    [4_000, "now"],
    [5_000, "5s ago"],
    [59_000, "59s ago"],
    [60_000, "1m ago"],
    [90_000, "2m ago"],
    [300_000, "5m ago"],
  ])("writes %sms before the latest reading as %s", (before, expected) => {
    const latest = 1_000_000;
    expect(ago(latest - before, latest)).toBe(expected);
  });
});

describe("marks", () => {
  it("steps every fifteen seconds over a minute or less", () => {
    expect(marks([0, 60_000])).toEqual([0, 15_000, 30_000, 45_000, 60_000]);
  });

  it("steps every thirty seconds up to three minutes", () => {
    expect(marks([0, 120_000])).toEqual([0, 30_000, 60_000, 90_000, 120_000]);
  });

  it("steps every minute past that", () => {
    expect(marks([0, 300_000])).toEqual([
      0, 60_000, 120_000, 180_000, 240_000, 300_000,
    ]);
  });

  it("counts back from the latest reading, not up from the oldest", () => {
    // 50 seconds of history: ticks at now, 15s ago, 30s ago, 45s ago, and
    // none at the oldest reading since it is not on the step.
    expect(marks([10_000, 60_000])).toEqual([15_000, 30_000, 45_000, 60_000]);
  });

  it("marks a lone reading once", () => {
    expect(marks([5_000, 5_000])).toEqual([5_000]);
  });
});
