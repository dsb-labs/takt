<script setup lang="ts">
import { useServices } from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import SortHeader from "../../components/SortHeader.vue";
import { pluralize } from "../../format";
import { useQueryFilter } from "../../filter";
import { useSort } from "../../sort";

const { filter, queries } = useQueryFilter();
const services = useServices(() => queries.value);

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
              name="port"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Target</SortHeader
            >
            <SortHeader
              name="backends"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Backends</SortHeader
            >
          </tr>
        </thead>
        <tbody>
          <tr v-if="sort.sorted.value.length === 0">
            <td
              colspan="3"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No services. Apply a service manifest with the CLI to create one.
            </td>
          </tr>
          <tr
            v-for="service in sort.sorted.value"
            :key="service.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/services/${service.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
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
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
