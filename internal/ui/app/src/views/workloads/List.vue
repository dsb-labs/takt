<script setup lang="ts">
import { computed } from "vue";

import { useWorkloads } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ListTable from "../../components/ListTable.vue";
import QueryInput from "../../components/QueryInput.vue";
import StateBadge from "../../components/StateBadge.vue";
import { relativeTime, healthStyles } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";
import type { Workload, WorkloadState } from "../../api/types";

const { filter, queries } = useQueryFilter();
const workloads = useWorkloads(() => queries.value);

function runningInstances(workload: Workload): string {
  const instances = workload.instances ?? [];
  const running = instances.filter((i) => i.state === "running").length;
  return `${running}/${workload.spec.count ?? 1}`;
}

// health summarises the checks across a workload's instances: the worst
// answer wins, and a workload with no checks reports nothing.
function health(workload: Workload): { label: string; style: string } | null {
  const statuses = (workload.instances ?? [])
    .map((i) => i.health?.status)
    .filter((s) => s !== undefined);
  if (statuses.length === 0) return null;

  if (statuses.includes("unhealthy")) {
    return { label: "unhealthy", style: healthStyles.unhealthy };
  }
  if (statuses.includes("starting")) {
    return { label: "starting", style: healthStyles.starting };
  }
  return { label: "healthy", style: healthStyles.healthy };
}

// counts summarises the listed workloads by state, in a fixed order so the
// chips do not shuffle as states come and go.
const counts = computed(() => {
  const order: WorkloadState[] = [
    "running",
    "degraded",
    "pending",
    "failed",
    "stopped",
    "completed",
    "suspended",
    "terminating",
  ];
  const byState = new Map<WorkloadState, number>();
  for (const workload of workloads.data.value ?? []) {
    byState.set(workload.state, (byState.get(workload.state) ?? 0) + 1);
  }
  return order
    .filter((state) => byState.has(state))
    .map((state) => ({ state, count: byState.get(state)! }));
});

// A phone shows the name and the state. The rest of the columns arrive
// with the room to read them.
const columns = [
  { name: "name", label: "Name" },
  { name: "state", label: "State" },
  { name: "health", label: "Health", class: "hidden sm:table-cell" },
  { name: "runtime", label: "Runtime", class: "hidden md:table-cell" },
  { name: "instances", label: "Instances", class: "hidden md:table-cell" },
  { name: "nextRun", label: "Next run", class: "hidden md:table-cell" },
];

const sort = useSort(() => workloads.data.value, "name", {
  name: (w) => w.name,
  state: (w) => w.state,
  health: (w) => health(w)?.label ?? "",
  runtime: (w) => w.runtime,
  instances: (w) =>
    (w.instances ?? []).filter((i) => i.state === "running").length,
  nextRun: (w) => w.nextRun ?? "",
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await workloads.suspense().catch(() => {});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Workloads</h1>
      <QueryInput v-model="filter" />
    </div>

    <div v-if="counts.length" class="mt-4 flex flex-wrap gap-2">
      <span
        v-for="entry in counts"
        :key="entry.state"
        class="inline-flex items-center gap-2 rounded-md border border-slate-200 bg-white py-1 pr-2.5 pl-1 text-sm dark:border-slate-800 dark:bg-slate-900"
      >
        <StateBadge :state="entry.state" />
        {{ entry.count }}
      </span>
    </div>

    <ErrorBanner
      v-if="workloads.isError.value"
      :message="`Failed to list workloads: ${workloads.error.value?.message}`"
    />

    <ListTable
      v-else
      :columns="columns"
      :sort="sort"
      :row-key="(w) => w.name"
      empty="No workloads."
    >
      <template #row="{ item: workload }">
        <td class="px-4 py-3 font-medium">
          <RouterLink
            :to="`/workloads/${workload.name}`"
            class="text-pulse-700 dark:text-pulse-300 hover:underline"
          >
            {{ workload.name }}
          </RouterLink>
        </td>
        <td class="px-4 py-3">
          <StateBadge :state="workload.state" />
          <span
            v-if="workload.lastError"
            class="ml-1.5 inline-block h-2 w-2 rounded-full bg-rose-500 align-middle"
            :title="workload.lastError"
          ></span>
        </td>
        <td class="hidden px-4 py-3 sm:table-cell">
          <span v-if="health(workload)" :class="health(workload)!.style">
            {{ health(workload)!.label }}
          </span>
          <span v-else class="text-slate-400 dark:text-slate-500">—</span>
        </td>
        <td
          class="hidden px-4 py-3 text-slate-600 md:table-cell dark:text-slate-400"
        >
          {{ workload.runtime }}
        </td>
        <td
          class="hidden px-4 py-3 text-slate-600 md:table-cell dark:text-slate-400"
        >
          {{ runningInstances(workload) }}
        </td>
        <td
          class="hidden px-4 py-3 text-slate-600 md:table-cell dark:text-slate-400"
        >
          {{ workload.nextRun ? relativeTime(workload.nextRun) : "—" }}
        </td>
      </template>
    </ListTable>
  </div>
</template>
