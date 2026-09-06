<script setup lang="ts">
import { useServices } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ListTable from "../../components/ListTable.vue";
import QueryInput from "../../components/QueryInput.vue";
import { pluralize } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const services = useServices(() => queries.value);

const columns = [
  { name: "name", label: "Name" },
  { name: "port", label: "Target" },
  { name: "backends", label: "Backends" },
];

const sort = useSort(() => services.data.value, "name", {
  name: (s) => s.name,
  port: (s) => s.target.port,
  backends: (s) => s.backends?.length ?? 0,
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await services.suspense().catch(() => {});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Services</h1>
      <QueryInput v-model="filter" />
    </div>

    <ErrorBanner
      v-if="services.isError.value"
      :message="`Failed to list services: ${services.error.value?.message}`"
    />

    <ListTable
      v-else
      :columns="columns"
      :sort="sort"
      :row-key="(s) => s.name"
      empty="No services."
    >
      <template #row="{ item: service }">
        <td class="px-4 py-3 font-medium">
          <RouterLink
            :to="`/services/${service.name}`"
            class="text-pulse-700 dark:text-pulse-300 hover:underline"
          >
            {{ service.name }}
          </RouterLink>
        </td>
        <td
          class="px-4 py-3 font-mono text-xs text-slate-600 dark:text-slate-400"
        >
          {{ service.target.port }}/{{ service.target.protocol ?? "tcp" }}
        </td>
        <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
          {{
            service.backends?.length
              ? pluralize(service.backends.length, "backend")
              : "none"
          }}
        </td>
      </template>
    </ListTable>
  </div>
</template>
