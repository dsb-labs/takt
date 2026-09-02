<script setup lang="ts">
import { useVolumes } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import SortHeader from "../../components/SortHeader.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const volumes = useVolumes(() => queries.value);

const sort = useSort(() => volumes.data.value, "name", {
  name: (v) => v.name,
  created: (v) => v.createdAt,
  usedBy: (v) => v.usedBy?.length ?? 0,
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await volumes.suspense().catch(() => {});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Volumes</h1>
      <QueryInput v-model="filter" />
    </div>

    <ErrorBanner
      v-if="volumes.isError.value"
      :message="`Failed to list volumes: ${volumes.error.value?.message}`"
    />

    <div
      v-else
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
          >
            <SortHeader
              name="name"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Name</SortHeader
            >
            <SortHeader
              name="created"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Created</SortHeader
            >
            <SortHeader
              name="usedBy"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Used by</SortHeader
            >
          </tr>
        </thead>
        <tbody>
          <tr v-if="sort.sorted.value.length === 0">
            <td
              colspan="3"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No volumes. Apply a volume manifest with the CLI to create one.
            </td>
          </tr>
          <tr
            v-for="volume in sort.sorted.value"
            :key="volume.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/volumes/${volume.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
              >
                {{ volume.name }}
              </RouterLink>
            </td>
            <td
              class="px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="absoluteTime(volume.createdAt)"
            >
              {{ relativeTime(volume.createdAt) }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{
                volume.usedBy?.length
                  ? pluralize(volume.usedBy.length, "workload")
                  : "unused"
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
