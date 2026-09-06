<script setup lang="ts">
import { useVariables } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ListTable from "../../components/ListTable.vue";
import QueryInput from "../../components/QueryInput.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const variables = useVariables(() => queries.value);

const columns = [
  { name: "name", label: "Name" },
  { name: "updated", label: "Updated" },
  { name: "usedBy", label: "Used by" },
];

const sort = useSort(() => variables.data.value, "name", {
  name: (v) => v.name,
  updated: (v) => v.updatedAt,
  usedBy: (v) => v.usedBy?.length ?? 0,
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await variables.suspense().catch(() => {});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Variables</h1>
      <div class="flex items-center gap-3">
        <QueryInput v-model="filter" />
        <RouterLink
          to="/variables/new"
          class="bg-pulse-600 hover:bg-pulse-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
        >
          Set a variable
        </RouterLink>
      </div>
    </div>

    <ErrorBanner
      v-if="variables.isError.value"
      :message="`Failed to list variables: ${variables.error.value?.message}`"
    />

    <ListTable
      v-else
      :columns="columns"
      :sort="sort"
      :row-key="(v) => v.name"
      empty="No variables."
    >
      <template #row="{ item: variable }">
        <td class="px-4 py-3 font-medium">
          <RouterLink
            :to="`/variables/${variable.name}`"
            class="text-pulse-700 dark:text-pulse-300 hover:underline"
          >
            {{ variable.name }}
          </RouterLink>
        </td>
        <td
          class="px-4 py-3 text-slate-600 dark:text-slate-400"
          :title="absoluteTime(variable.updatedAt)"
        >
          {{ relativeTime(variable.updatedAt) }}
        </td>
        <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
          {{
            variable.usedBy?.length
              ? pluralize(variable.usedBy.length, "workload")
              : "unused"
          }}
        </td>
      </template>
    </ListTable>
  </div>
</template>
