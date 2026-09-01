<script setup lang="ts">
import { useSecrets } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import SortHeader from "../../components/SortHeader.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const secrets = useSecrets(() => queries.value);

const sort = useSort(() => secrets.data.value, "name", {
  name: (s) => s.name,
  revision: (s) => s.revision,
  updated: (s) => s.updatedAt,
  usedBy: (s) => s.usedBy?.length ?? 0,
});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Secrets</h1>
      <div class="flex items-center gap-3">
        <QueryInput v-model="filter" />
        <RouterLink
          to="/secrets/new"
          class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
        >
          Set a secret
        </RouterLink>
      </div>
    </div>

    <ErrorBanner
      v-if="secrets.isError.value"
      :message="`Failed to list secrets: ${secrets.error.value?.message}`"
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
              name="revision"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Revision</SortHeader
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
              colspan="4"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No secrets.
            </td>
          </tr>
          <tr
            v-for="secret in sort.sorted.value"
            :key="secret.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/secrets/${secret.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
              >
                {{ secret.name }}
              </RouterLink>
            </td>
            <td
              class="px-4 py-3 font-mono text-xs text-slate-500 dark:text-slate-400"
            >
              {{ secret.revision }}
            </td>
            <td
              class="px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="absoluteTime(secret.updatedAt)"
            >
              {{ relativeTime(secret.updatedAt) }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{
                secret.usedBy?.length
                  ? pluralize(secret.usedBy.length, "workload")
                  : "unused"
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
