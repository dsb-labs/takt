import { ref, watch, type Ref } from "vue";

import type { Instance } from "./api/types";

// A rolling history of what the instances of a workload are consuming,
// gathered from the readings the workload view already polls for.
//
// The server reports an instant rather than a history: it holds one previous
// sample per instance, enough to work a rate out from, and nothing before
// that. A page that wants a line rather than a number has to remember what it
// has been shown, which is what this does. A reload starts the history over,
// which is honest about where it came from.

// How much history a chart holds. Long enough to show a workload settling
// after a restart, short enough that the page is not carrying an afternoon
// of readings around.
const retention = 5 * 60 * 1000;

export type Point = { x: number; y: number };

export type Series = {
  // The instance the line belongs to, as its driver ID.
  id: string;
  // What the line is called in the legend: the instance's index.
  label: string;
  points: Point[];
};

export type Usage = {
  memory: Series[];
  cpu: Series[];
  // The limits the instances answer to, which every instance of a workload
  // shares because they come from the one specification. Undefined when the
  // specification named none.
  memoryLimit?: number;
  cpuLimit?: number;
};

// useUsageSeries accumulates the readings of each poll into a series per
// instance, so a view can chart what it has been shown so far.
export function useUsageSeries(instances: () => Instance[] | undefined): {
  usage: Ref<Usage>;
} {
  const usage = ref<Usage>({ memory: [], cpu: [] });

  watch(
    instances,
    (current) => {
      const at = Date.now();
      const reading = (current ?? []).filter((instance) => instance.usage);

      usage.value = {
        memory: extend(usage.value.memory, reading, at, (i) => i.usage?.memory),
        cpu: extend(usage.value.cpu, reading, at, (i) => i.usage?.cpu),
        memoryLimit: reading.find((i) => i.usage?.memoryLimit)?.usage
          ?.memoryLimit,
        cpuLimit: reading.find((i) => i.usage?.cpuLimit)?.usage?.cpuLimit,
      };
    },
    { immediate: true, deep: true },
  );

  return { usage };
}

// extend appends this poll's readings to the series it already has, drops
// what has aged out of the retained span, and forgets an instance that is no longer
// reported at all — a replaced instance leaves with its line rather than
// leaving a flat one behind.
function extend(
  existing: Series[],
  instances: Instance[],
  at: number,
  read: (instance: Instance) => number | undefined,
): Series[] {
  const previous = new Map(existing.map((series) => [series.id, series]));
  const oldest = at - retention;

  return instances
    .map((instance) => {
      const points = previous.get(instance.id)?.points ?? [];
      const value = read(instance);

      return {
        id: instance.id,
        label: `Instance ${instance.index ?? 0}`,
        points:
          // A reading the server has not worked out yet, which a CPU rate is
          // until a second sample, leaves the line alone rather than drawing
          // it through zero.
          value === undefined
            ? points.filter((point) => point.x >= oldest)
            : [...points, { x: at, y: value }].filter(
                (point) => point.x >= oldest,
              ),
      };
    })
    .filter((series) => series.points.length > 0);
}
