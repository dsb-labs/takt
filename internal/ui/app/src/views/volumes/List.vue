<script setup lang="ts">
import { useVolumes } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ListTable from "../../components/ListTable.vue";
import QueryInput from "../../components/QueryInput.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const volumes = useVolumes(() => queries.value);

const columns = [
  { name: "name", label: "Name" },
  { name: "created", label: "Created" },
  { name: "usedBy", label: "Used by" },
];

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

    <ListTable
      v-else
      :columns="columns"
      :sort="sort"
      :row-key="(v) => v.name"
      empty="No volumes."
    >
      <template #row="{ item: volume }">
        <td class="px-4 py-3 font-medium">
          <RouterLink
            :to="`/volumes/${volume.name}`"
            class="text-pulse-700 dark:text-pulse-300 hover:underline"
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
      </template>
    </ListTable>
  </div>
</template>
