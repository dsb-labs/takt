import { computed, ref, watch, type Ref } from "vue";

import type { Instance } from "./api/types";

// A rolling history of what the instances of a workload are consuming,
// gathered from the readings a view already polls for.
//
// The server reports an instant rather than a history: it holds one previous
// sample per instance, enough to work a rate out from, and nothing before
// that. A page that wants a line rather than a number has to remember what it
// has been shown, which is what this does. A reload starts the history over,
// which is honest about where it came from.
//
// The history lives here rather than in the view holding the chart, so that
// moving between a workload and one of its instances keeps the lines that
// have been gathered. A view that owned them would start over on every
// navigation, and the chart would be at its emptiest exactly when a reader
// has just clicked through to look at it.

// How much history a chart holds. Long enough to show a workload settling
// after a restart, short enough that the page is not carrying an afternoon
// of readings around.
const retention = 5 * 60 * 1000;

export type Point = { x: number; y: number };

export type Series = {
  // The instance the line belongs to, as its driver ID.
  id: string;
  // The instance's index, which is what a line is named and coloured by.
  index: number;
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

const empty: Usage = { memory: [], cpu: [] };

// The history of every workload a reader has looked at this visit, keyed by
// name, and a counter bumped whenever one of them changes. The map is not
// reactive and replacing an entry in it is not a change Vue can see, so the
// counter is what a chart watches to know a poll has extended its lines.
const histories = new Map<string, { usage: Usage; touched: number }>();
const revision = ref(0);

// useUsageSeries accumulates the readings of each poll into a series per
// instance, so a view can chart what it has been shown so far.
export function useUsageSeries(
  workload: () => string,
  instances: () => Instance[] | undefined,
): Ref<Usage> {
  watch(
    instances,
    (current) => {
      const name = workload();
      const at = Date.now();
      const reading = (current ?? []).filter((instance) => instance.usage);
      const held = histories.get(name)?.usage ?? empty;

      histories.set(name, {
        touched: at,
        usage: {
          memory: extend(held.memory, reading, at, (i) => i.usage?.memory),
          cpu: extend(held.cpu, reading, at, (i) => i.usage?.cpu),
          memoryLimit: reading.find((i) => i.usage?.memoryLimit)?.usage
            ?.memoryLimit,
          cpuLimit: reading.find((i) => i.usage?.cpuLimit)?.usage?.cpuLimit,
        },
      });

      // A workload nothing has polled for longer than the history is worth
      // holding has none worth holding, and the map would otherwise grow for
      // as long as the tab is open.
      for (const [key, entry] of histories) {
        if (at - entry.touched > retention) histories.delete(key);
      }

      revision.value++;
    },
    { immediate: true, deep: true },
  );

  return computed(() => {
    void revision.value;

    return histories.get(workload())?.usage ?? empty;
  });
}

// seriesOf picks one instance's line out of a workload's history, which is
// what a page about a single instance charts.
export function seriesOf(series: Series[], index: number): Series[] {
  return series.filter((line) => line.index === index);
}

// extend appends this poll's readings to the series it already has, drops
// what has aged out of the retained span, and forgets an instance that is no
// longer reported at all — a replaced instance leaves with its line rather
// than leaving a flat one behind.
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
        index: instance.index ?? 0,
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
