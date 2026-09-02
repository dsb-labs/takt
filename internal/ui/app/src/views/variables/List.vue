<script setup lang="ts">
import { useVariables } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import SortHeader from "../../components/SortHeader.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const variables = useVariables(() => queries.value);

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
          class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
        >
          Set a variable
        </RouterLink>
      </div>
    </div>

    <ErrorBanner
      v-if="variables.isError.value"
      :message="`Failed to list variables: ${variables.error.value?.message}`"
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
              name="updated"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Updated</SortHeader
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
              No variables.
            </td>
          </tr>
          <tr
            v-for="variable in sort.sorted.value"
            :key="variable.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/variables/${variable.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
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
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
