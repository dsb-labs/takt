import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick, ref } from "vue";

import type { Instance } from "@/api/types";
import { seriesOf, useUsageSeries } from "@/composables/series";

function instance(index: number, usage?: Partial<Instance["usage"]>): Instance {
  return {
    id: `id-${index}`,
    index,
    state: "running",
    specHash: "abc",
    usage: usage ? { memory: 0, pids: 1, ...usage } : undefined,
  };
}

// Each test names a workload of its own, since the history is kept at the
// module root and would otherwise carry from one test into the next.
let name = 0;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
  name++;
});

afterEach(() => vi.useRealTimers());

// gather starts a history for a fresh workload and returns the handles a
// test drives it with: the instances to replace, and a step that advances
// the clock and lets the watcher run.
function gather(initial: Instance[]) {
  const instances = ref<Instance[] | undefined>(initial);
  const usage = useUsageSeries(
    () => `workload-${name}`,
    () => instances.value,
  );

  const poll = async (next: Instance[], after = 5000) => {
    vi.advanceTimersByTime(after);
    instances.value = next;
    await nextTick();
  };

  return { usage, poll };
}

describe("useUsageSeries", () => {
  it("starts a line for every instance reporting usage", () => {
    const { usage } = gather([
      instance(0, { memory: 100, cpu: 0.5 }),
      instance(1, { memory: 200, cpu: 0.25 }),
      instance(2),
    ]);

    expect(usage.value.memory.map((line) => line.label)).toEqual([
      "Instance 0",
      "Instance 1",
    ]);
    expect(usage.value.memory[0]?.points).toEqual([{ x: Date.now(), y: 100 }]);
    expect(usage.value.cpu[1]?.points).toEqual([{ x: Date.now(), y: 0.25 }]);
  });

  it("extends each line with every poll", async () => {
    const { usage, poll } = gather([instance(0, { memory: 100 })]);
    const first = Date.now();

    await poll([instance(0, { memory: 150 })]);

    expect(usage.value.memory[0]?.points).toEqual([
      { x: first, y: 100 },
      { x: first + 5000, y: 150 },
    ]);
  });

  it("leaves a line alone until the server has a figure", async () => {
    const { usage, poll } = gather([instance(0, { memory: 100 })]);

    expect(usage.value.cpu).toEqual([]);

    await poll([instance(0, { memory: 100, cpu: 0.1 })]);

    expect(usage.value.cpu[0]?.points).toEqual([{ x: Date.now(), y: 0.1 }]);
  });

  it("carries the limits the instances answer to", () => {
    const { usage } = gather([
      instance(0, { memory: 100, memoryLimit: 1024, cpu: 0.1, cpuLimit: 2 }),
    ]);

    expect(usage.value.memoryLimit).toBe(1024);
    expect(usage.value.cpuLimit).toBe(2);
  });

  it("reports no limit when none was named", () => {
    const { usage } = gather([instance(0, { memory: 100 })]);

    expect(usage.value.memoryLimit).toBeUndefined();
    expect(usage.value.cpuLimit).toBeUndefined();
  });

  it("forgets an instance that is no longer reported", async () => {
    const { usage, poll } = gather([
      instance(0, { memory: 100 }),
      instance(1, { memory: 100 }),
    ]);

    await poll([instance(0, { memory: 100 })]);

    expect(usage.value.memory.map((line) => line.id)).toEqual(["id-0"]);
  });

  it("drops readings older than the retained span", async () => {
    const { usage, poll } = gather([instance(0, { memory: 100 })]);

    await poll([instance(0, { memory: 200 })], 4 * 60 * 1000);
    await poll([instance(0, { memory: 300 })], 2 * 60 * 1000);

    expect(usage.value.memory[0]?.points.map((point) => point.y)).toEqual([
      200, 300,
    ]);
  });

  it("holds nothing for a workload with no instances", () => {
    const { usage } = gather([]);

    expect(usage.value).toEqual({ memory: [], cpu: [] });
  });
});

describe("seriesOf", () => {
  it("picks the line of one instance by its index", () => {
    const { usage } = gather([
      instance(0, { memory: 100 }),
      instance(1, { memory: 200 }),
    ]);

    expect(seriesOf(usage.value.memory, 1).map((line) => line.id)).toEqual([
      "id-1",
    ]);
  });
});
