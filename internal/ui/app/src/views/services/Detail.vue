<script setup lang="ts">
import { computed } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteService } from "../../api/mutations";
import { useService } from "../../api/queries";
import DeleteControl from "../../components/DeleteControl.vue";
import DetailCard from "../../components/DetailCard.vue";
import LabelsCard from "../../components/LabelsCard.vue";
import SortHeader from "../../components/SortHeader.vue";
import { absoluteTime, relativeTime } from "../../format";
import { labelQuery } from "../../filter";
import { useSort } from "../../sort";

const route = useRoute();
const router = useRouter();
const name = computed(() => route.params.name as string);

const service = useService(() => name.value);
const deleteService = useDeleteService(() => name.value);

const backendSort = useSort(() => service.data.value?.backends, "instance", {
  workload: (b) => b.workload,
  instance: (b) => b.instance,
  address: (b) => b.address,
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await service.suspense().catch(() => {});
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/services" class="hover:underline">Services</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <div
      v-if="service.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to read the service: {{ service.error.value?.message }}
    </div>

    <template v-else-if="service.data.value">
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="text-xl font-semibold">{{ service.data.value.name }}</h1>
        <div class="ml-auto">
          <DeleteControl
            :subject="`the service ${name}`"
            :remove="(force) => deleteService.mutateAsync(force)"
            @deleted="router.push('/services')"
          />
        </div>
      </header>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Target port
              </dt>
              <dd class="font-mono text-xs leading-5">
                {{ service.data.value.target.port }}/{{
                  service.data.value.target.protocol ?? "tcp"
                }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Created
              </dt>
              <dd :title="absoluteTime(service.data.value.createdAt)">
                {{ relativeTime(service.data.value.createdAt) }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Updated
              </dt>
              <dd :title="absoluteTime(service.data.value.updatedAt)">
                {{ relativeTime(service.data.value.updatedAt) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <DetailCard title="Selects">
          <div class="flex flex-wrap gap-2 px-4 py-3">
            <RouterLink
              v-for="(value, key) in service.data.value.target.labels"
              :key="key"
              :to="{ path: '/', query: { query: labelQuery(key, value) } }"
              class="hover:border-ocean-300 hover:text-ocean-700 dark:hover:border-ocean-700 dark:hover:text-ocean-300 rounded-md border border-slate-200 bg-slate-50 px-2.5 py-1 font-mono text-xs text-slate-700 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300"
              :title="`Show the workloads labelled ${key}=${value}`"
            >
              {{ key }}={{ value }}
            </RouterLink>
          </div>
        </DetailCard>

        <DetailCard title="Backends">
          <p
            v-if="!service.data.value.backends?.length"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            No backends. Nothing the target selects is running and fit to serve.
          </p>
          <div v-else class="overflow-x-auto">
            <table class="w-full text-left text-sm">
              <thead>
                <tr
                  class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
                >
                  <SortHeader
                    v-for="[column, label] in [
                      ['workload', 'Workload'],
                      ['instance', 'Instance'],
                      ['address', 'Address'],
                    ]"
                    :key="column"
                    :name="column!"
                    :sort-key="backendSort.key.value"
                    :descending="backendSort.descending.value"
                    @sort="backendSort.toggle"
                    >{{ label }}</SortHeader
                  >
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="backend in backendSort.sorted.value"
                  :key="`${backend.workload}-${backend.instance}`"
                  class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
                >
                  <td class="px-4 py-2.5 font-medium">
                    <RouterLink
                      :to="`/workloads/${backend.workload}`"
                      class="text-ocean-700 dark:text-ocean-300 hover:underline"
                    >
                      {{ backend.workload }}
                    </RouterLink>
                  </td>
                  <td class="px-4 py-2.5 text-slate-600 dark:text-slate-400">
                    {{ backend.instance }}
                  </td>
                  <td
                    class="px-4 py-2.5 font-mono text-xs text-slate-600 dark:text-slate-400"
                  >
                    {{ backend.address }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </DetailCard>

        <LabelsCard :labels="service.data.value.labels" target="/services" />
      </div>
    </template>
  </div>
</template>
