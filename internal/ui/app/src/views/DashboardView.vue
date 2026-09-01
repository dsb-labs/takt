<script setup lang="ts">
import { useWorkloads } from "../api/queries";
import StateBadge from "../components/StateBadge.vue";
import { relativeTime } from "../format";
import type { Workload } from "../api/types";

const workloads = useWorkloads();

function runningInstances(workload: Workload): string {
  const instances = workload.instances ?? [];
  const running = instances.filter((i) => i.state === "running").length;
  return `${running}/${workload.spec.count ?? 1}`;
}
</script>

<template>
  <div>
    <h1 class="text-xl font-semibold">Workloads</h1>
    <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
      Everything the server holds a specification for, and what each one is
      doing.
    </p>

    <div
      v-if="workloads.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to list workloads: {{ workloads.error.value?.message }}
    </div>

    <div
      v-else
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">State</th>
            <th class="px-4 py-3 font-medium">Runtime</th>
            <th class="px-4 py-3 font-medium">Instances</th>
            <th class="px-4 py-3 font-medium">Next run</th>
            <th class="px-4 py-3 font-medium">Last error</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="workloads.isPending.value">
            <td
              colspan="6"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              Loading…
            </td>
          </tr>
          <tr v-else-if="workloads.data.value?.length === 0">
            <td
              colspan="6"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No workloads. Apply a manifest with the CLI to create one.
            </td>
          </tr>
          <tr
            v-for="workload in workloads.data.value"
            :key="workload.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/workloads/${workload.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
              >
                {{ workload.name }}
              </RouterLink>
            </td>
            <td class="px-4 py-3"><StateBadge :state="workload.state" /></td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{ workload.runtime }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{ runningInstances(workload) }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{ workload.nextRun ? relativeTime(workload.nextRun) : "—" }}
            </td>
            <td
              class="max-w-md truncate px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="workload.lastError"
            >
              {{ workload.lastError ?? "—" }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
