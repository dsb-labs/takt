<script setup lang="ts">
import { computed } from "vue";

import type { WorkloadState } from "../api/types";

const props = defineProps<{ state: WorkloadState }>();

const styles: Record<WorkloadState, string> = {
  running:
    "bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300",
  degraded: "bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
  pending: "bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300",
  terminating:
    "bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300",
  stopped: "bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300",
  completed:
    "bg-ocean-100 text-ocean-800 dark:bg-ocean-900 dark:text-ocean-200",
  failed: "bg-rose-100 text-rose-800 dark:bg-rose-950 dark:text-rose-300",
  suspended:
    "bg-violet-100 text-violet-800 dark:bg-violet-950 dark:text-violet-300",
};

const style = computed(() => styles[props.state]);
</script>

<template>
  <span
    class="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium"
    :class="style"
  >
    {{ state }}
  </span>
</template>
