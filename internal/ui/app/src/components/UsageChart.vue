<script setup lang="ts">
import {
  Chart,
  LinearScale,
  LineElement,
  PointElement,
  Tooltip,
  type ChartData,
  type ChartOptions,
} from "chart.js";
import { computed, onUnmounted, ref } from "vue";
import { Line } from "vue-chartjs";

import type { Series } from "../series";

// Only the pieces a line chart draws with, so the bundle carries the chart
// types this page uses rather than every chart the library can draw.
Chart.register(LinearScale, LineElement, PointElement, Tooltip);

const props = defineProps<{
  series: Series[];
  // The limit the readings answer to, drawn as a dashed line across the
  // chart. Undefined when the specification named none, which leaves the
  // chart scaled to what is being used.
  limit?: number;
  // How a figure is written on an axis, in a tooltip and in the legend.
  format: (value: number) => string;
}>();

// The chart is drawn in the colours of whichever scheme the page is in, and
// redrawn when that changes, so a reader switching their system theme is not
// left with a chart lit for the other one.
const scheme = globalThis.matchMedia("(prefers-color-scheme: dark)");
const dark = ref(scheme.matches);

const onScheme = (event: MediaQueryListEvent) => (dark.value = event.matches);
scheme.addEventListener("change", onScheme);
onUnmounted(() => scheme.removeEventListener("change", onScheme));

// The accents a line is drawn in, taken in order, so instance zero is the
// same colour on the memory chart as it is on the CPU one. They repeat past
// the sixth instance, which is more lines than a chart this size reads well
// with anyway.
const accents = [
  "oklch(0.65 0.12 81)", // pulse
  "oklch(0.7 0.15 162)", // emerald
  "oklch(0.68 0.15 237)", // sky
  "oklch(0.65 0.2 300)", // violet
  "oklch(0.75 0.15 70)", // amber
  "oklch(0.7 0.16 20)", // rose
];

const limitColour = "oklch(0.64 0.21 25)";

const grid = computed(() =>
  dark.value ? "oklch(0.279 0.002 68)" : "oklch(0.929 0.012 82)",
);
const text = computed(() =>
  dark.value ? "oklch(0.704 0.004 78)" : "oklch(0.554 0.003 75)",
);

const data = computed<ChartData<"line">>(() => {
  const lines = props.series.map((series, index) => ({
    label: series.label,
    data: series.points,
    borderColor: accents[index % accents.length],
    backgroundColor: accents[index % accents.length],
    borderWidth: 2,
    pointRadius: 0,
    pointHitRadius: 8,
    tension: 0.3,
  }));

  // The limit is a line of its own rather than an annotation, drawn from the
  // first reading to the last so it spans whatever the chart is showing.
  const span = extent(props.series);
  if (props.limit !== undefined && span) {
    lines.push({
      label: "Limit",
      data: span.map((x) => ({ x, y: props.limit as number })),
      borderColor: limitColour,
      backgroundColor: limitColour,
      borderWidth: 1.5,
      // The dashes are what tell a reader the line is a bound rather than a
      // seventh instance.
      borderDash: [4, 4],
      pointRadius: 0,
      pointHitRadius: 0,
      tension: 0,
    } as (typeof lines)[number]);
  }

  return { datasets: lines };
});

// extent reports the first and last instant any series has a reading at,
// which is how far the limit line has to reach.
function extent(series: Series[]): [number, number] | null {
  const instants = series.flatMap((line) => line.points.map((p) => p.x));
  if (!instants.length) return null;

  return [Math.min(...instants), Math.max(...instants)];
}

// ago writes an instant as how long before the latest reading it was, which is
// what a rolling window wants on its axis: "now", "2m ago".
function ago(instant: number, latest: number): string {
  const seconds = Math.round((latest - instant) / 1000);
  if (seconds < 5) return "now";
  if (seconds < 60) return `${seconds}s ago`;

  return `${Math.round(seconds / 60)}m ago`;
}

// marks places a tick every so often back from the latest reading, rather
// than leaving the library to pick round numbers of milliseconds. Two of
// those can land inside the same second of history and both read "1m ago",
// and the ticks it adds past the ends leave the lines short of the walls.
function marks(span: [number, number]): number[] {
  const [oldest, latest] = span;
  const step =
    latest - oldest <= 60_000
      ? 15_000
      : latest - oldest <= 180_000
        ? 30_000
        : 60_000;

  const ticks: number[] = [];
  for (let at = latest; at >= oldest; at -= step) ticks.push(at);

  return ticks.reverse();
}

const options = computed<ChartOptions<"line">>(() => {
  // The axis runs from the first reading to the last, so the lines reach both
  // walls rather than starting a tick in from each.
  const span = extent(props.series) ?? [0, 1];
  const latest = span[1];

  return {
    responsive: true,
    maintainAspectRatio: false,
    // A poll every few seconds would otherwise animate the whole chart each
    // time, which reads as a wobble rather than as new data.
    animation: false,
    interaction: { mode: "nearest", axis: "x", intersect: false },
    scales: {
      x: {
        type: "linear",
        min: span[0],
        max: latest,
        offset: false,
        grid: { display: false },
        border: { color: grid.value },
        afterBuildTicks: (axis) => {
          axis.ticks = marks(span).map((value) => ({ value }));
        },
        ticks: {
          color: text.value,
          maxRotation: 0,
          autoSkip: false,
          callback: (value) => ago(Number(value), latest),
        },
      },
      y: {
        beginAtZero: true,
        grid: { color: grid.value },
        border: { display: false },
        ticks: {
          color: text.value,
          callback: (value) => props.format(Number(value)),
        },
      },
    },
    plugins: {
      // The lines are named in the tooltip, and a workload running two or
      // three instances does not need a key for colours it can hover.
      legend: { display: false },
      tooltip: {
        callbacks: {
          title: (items) => ago(Number(items[0]?.parsed.x), latest),
          label: (item) =>
            `${item.dataset.label}: ${props.format(Number(item.parsed.y))}`,
        },
      },
    },
  };
});
</script>

<template>
  <!-- min-w-0 because the chart sizes itself to this container: a grid item
       is free to grow past its column by default, which leaves the canvas
       measuring a width the phone does not have and the page scrolling
       sideways. -->
  <div class="h-56 min-w-0 px-4 py-4">
    <p
      v-if="!series.length"
      class="flex h-full items-center justify-center text-sm text-slate-500 dark:text-slate-400"
    >
      Nothing is reporting usage yet.
    </p>
    <Line v-else :data="data" :options="options" />
  </div>
</template>
