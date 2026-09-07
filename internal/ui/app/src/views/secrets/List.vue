<script setup lang="ts">
import { useSecrets } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ListTable from "../../components/ListTable.vue";
import QueryInput from "../../components/QueryInput.vue";
import { absoluteTime, pluralize, relativeTime } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const secrets = useSecrets(() => queries.value);

const columns = [
  { name: "name", label: "Name" },
  { name: "revision", label: "Revision", class: "hidden md:table-cell" },
  { name: "updated", label: "Updated", class: "hidden sm:table-cell" },
  { name: "usedBy", label: "Used by" },
];

const sort = useSort(() => secrets.data.value, "name", {
  name: (s) => s.name,
  revision: (s) => s.revision,
  updated: (s) => s.updatedAt,
  usedBy: (s) => s.usedBy?.length ?? 0,
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await secrets.suspense().catch(() => {});
</script>

<template>
  <div>
    <!-- On a phone the title and the button share the first row and the
         filter takes a full row beneath, the way the views without a button
         lay out. On a wider screen everything sits on one row. -->
    <div class="flex flex-wrap items-center gap-3">
      <h1 class="text-xl font-semibold">Secrets</h1>
      <RouterLink
        to="/secrets/new"
        class="bg-pulse-600 hover:bg-pulse-700 order-2 ml-auto rounded-md px-3 py-1.5 text-sm font-medium whitespace-nowrap text-white sm:order-3 sm:ml-0"
      >
        Set a secret
      </RouterLink>
      <div class="order-3 w-full sm:order-2 sm:ml-auto sm:w-auto">
        <QueryInput v-model="filter" />
      </div>
    </div>

    <ErrorBanner
      v-if="secrets.isError.value"
      :message="`Failed to list secrets: ${secrets.error.value?.message}`"
    />

    <ListTable
      v-else
      :columns="columns"
      :sort="sort"
      :row-key="(s) => s.name"
      empty="No secrets."
    >
      <template #row="{ item: secret }">
        <td class="px-4 py-3 font-medium">
          <RouterLink
            :to="`/secrets/${secret.name}`"
            class="text-pulse-700 dark:text-pulse-300 hover:underline"
          >
            {{ secret.name }}
          </RouterLink>
        </td>
        <td
          class="hidden px-4 py-3 font-mono text-xs text-slate-500 md:table-cell dark:text-slate-400"
        >
          {{ secret.revision }}
        </td>
        <td
          class="hidden px-4 py-3 text-slate-600 sm:table-cell dark:text-slate-400"
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
      </template>
    </ListTable>
  </div>
</template>
