<script setup lang="ts">
import { computed } from "vue";

// A meter of one figure against the capacity it draws on. The fill carries
// how full it is, in the brass of the theme until the thresholds the usage
// rows warn at, and the track is a lighter step of the same ramp so the
// whole bar reads as one thing. The figures are written beneath it, since a
// bar alone says "some" and a reader wants the number.
const props = defineProps<{
  // The figure the bar fills to.
  value: number;
  // The capacity the figure is measured against.
  total: number;
  // What the figure is, written after the numbers: "allocated", "used".
  verb: string;
  // How a figure is written.
  format: (value: number) => string;
}>();

const fraction = computed(() =>
  props.total > 0 ? Math.min(props.value / props.total, 1) : 0,
);

const fill = computed(() => {
  if (fraction.value >= 0.95) return "bg-rose-500 dark:bg-rose-400";
  if (fraction.value >= 0.8) return "bg-amber-500 dark:bg-amber-400";
  return "bg-pulse-500 dark:bg-pulse-400";
});

const percentage = computed(() => Math.round(fraction.value * 100));

const summary = computed(
  () =>
    `${props.format(props.value)} of ${props.format(props.total)} ${props.verb}`,
);
</script>

<template>
  <div :title="`${summary} (${percentage}%)`">
    <div
      class="bg-pulse-100 dark:bg-pulse-950 h-2.5 w-full overflow-hidden rounded-full"
      role="meter"
      :aria-valuemin="0"
      :aria-valuemax="total"
      :aria-valuenow="value"
      :aria-label="summary"
    >
      <div
        class="h-full rounded-full transition-[width] duration-300"
        :class="fill"
        :style="{ width: `${fraction * 100}%` }"
      ></div>
    </div>
    <div
      class="mt-1.5 flex justify-between gap-4 text-xs text-slate-500 dark:text-slate-400"
    >
      <span>{{ summary }}</span>
      <span class="tabular-nums">{{ percentage }}%</span>
    </div>
  </div>
</template>
