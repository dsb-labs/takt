<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute } from "vue-router";

import { useWorkload } from "../../../api/queries";
import DetailCard from "../../../components/DetailCard.vue";
import DetailPage from "../../../components/DetailPage.vue";
import LogViewer from "../../../components/LogViewer.vue";
import OverviewRow from "../../../components/OverviewRow.vue";
import SortHeader from "../../../components/SortHeader.vue";
import StateBadge from "../../../components/StateBadge.vue";
import UsageChart from "../../../components/UsageChart.vue";
import {
  absoluteTime,
  bytes,
  cores,
  healthStyles,
  relativeTime,
} from "../../../format";
import { seriesOf, useUsageSeries } from "../../../series";
import { useSort } from "../../../sort";

const route = useRoute();
// Snapshots rather than computed reads, for the reason the workload view
// takes one: Suspense keeps this view on screen while the next loads, and a
// reactive read would watch the route it is leaving.
const name = route.params.name as string;
const index = Number(route.params.index);

// The instance is read out of the workload, which the page next door already
// polls and vue-query already holds. An instance is observed state rather
// than a resource of its own, so there is nothing else to ask for.
const workload = useWorkload(() => name);

const instance = computed(() =>
  workload.data.value?.instances?.find((each) => (each.index ?? 0) === index),
);

const usage = useUsageSeries(
  () => name,
  () => workload.data.value?.instances,
);

const memory = computed(() => seriesOf(usage.value.memory, index));
const cpu = computed(() => seriesOf(usage.value.cpu, index));

const copied = ref(false);

async function copyID() {
  const id = instance.value?.id;
  if (!id) return;

  await navigator.clipboard.writeText(id);
  copied.value = true;
  setTimeout(() => (copied.value = false), 1500);
}

// The ports this instance publishes. A workload running more than one gets a
// host port per instance, so the row that matters here is its own.
const ports = computed(() =>
  (workload.data.value?.ports ?? []).filter(
    (port) => (port.instance ?? 0) === index,
  ),
);

const portSort = useSort(() => ports.value, "name", {
  name: (p) => p.name ?? "",
  host: (p) => p.from,
  workload: (p) => p.to,
  protocol: (p) => p.protocol ?? "",
});

const host = window.location.hostname;

await workload.suspense().catch(() => {});
</script>

<template>
  <DetailPage
    section="Workloads"
    section-to="/"
    :trail="[{ label: name, to: `/workloads/${name}` }, { label: 'Instances' }]"
    :name="`${name} instance ${index}`"
    :crumb="String(index)"
    :error="
      workload.isError.value
        ? `Failed to read the workload: ${workload.error.value?.message}`
        : ''
    "
  >
    <template #header>
      <StateBadge v-if="instance" :state="instance.state" />
    </template>

    <!-- An instance a reader has a link to may since have been replaced, or
         the workload scaled down past it. The page says so rather than
         rendering empty cards. -->
    <p
      v-if="!instance"
      class="mt-6 rounded-lg border border-slate-200 bg-white px-4 py-6 text-sm text-slate-500 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-400"
    >
      The workload is not running an instance {{ index }}.
    </p>

    <template v-else>
      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview" class="min-w-0">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="ID" mono>
              <span class="inline-flex items-center gap-1.5">
                <span class="break-all">{{ instance.id }}</span>
                <button
                  class="hover:text-pulse-700 dark:hover:text-pulse-300 shrink-0 text-slate-400 dark:text-slate-500"
                  title="Copy the full ID"
                  @click="copyID"
                >
                  <svg
                    v-if="!copied"
                    class="h-3.5 w-3.5"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="2"
                    stroke-linecap="round"
                    stroke-linejoin="round"
                  >
                    <rect x="9" y="9" width="13" height="13" rx="2" />
                    <path
                      d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"
                    />
                  </svg>
                  <svg
                    v-else
                    class="h-3.5 w-3.5 text-emerald-600 dark:text-emerald-400"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="2"
                    stroke-linecap="round"
                    stroke-linejoin="round"
                  >
                    <path d="M20 6 9 17l-5-5" />
                  </svg>
                </button>
              </span>
            </OverviewRow>
            <OverviewRow label="State">{{ instance.state }}</OverviewRow>
            <OverviewRow
              v-if="instance.startedAt"
              label="Started"
              :title="absoluteTime(instance.startedAt)"
              >{{ relativeTime(instance.startedAt) }}</OverviewRow
            >
            <OverviewRow
              v-if="instance.exitCode !== undefined"
              label="Exit code"
              >{{ instance.exitCode }}
            </OverviewRow>
          </dl>
        </DetailCard>

        <DetailCard v-if="instance.health" title="Health" class="min-w-0">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Status
              </dt>
              <dd :class="healthStyles[instance.health.status]">
                {{ instance.health.status }}
              </dd>
            </div>
            <OverviewRow label="Failures">{{
              instance.health.failures ?? 0
            }}</OverviewRow>
            <OverviewRow
              v-if="instance.health.checkedAt"
              label="Checked"
              :title="absoluteTime(instance.health.checkedAt)"
              >{{ relativeTime(instance.health.checkedAt) }}</OverviewRow
            >
            <OverviewRow v-if="instance.health.error" label="Error">{{
              instance.health.error
            }}</OverviewRow>
          </dl>
        </DetailCard>
      </div>

      <!-- The two charts share a row of their own, so memory and CPU are read
           against each other rather than wherever the cards above them
           happen to end. -->
      <div class="mt-6 grid gap-6 sm:grid-cols-2">
        <DetailCard title="Memory" class="min-w-0">
          <UsageChart
            :series="memory"
            :limit="usage.memoryLimit"
            :format="bytes"
          />
        </DetailCard>

        <DetailCard title="CPU" class="min-w-0">
          <UsageChart :series="cpu" :limit="usage.cpuLimit" :format="cores" />
        </DetailCard>
      </div>

      <div v-if="ports.length" class="mt-6">
        <DetailCard title="Ports">
          <table class="w-full text-left text-sm">
            <thead>
              <tr
                class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
              >
                <SortHeader
                  v-for="[column, label] in [
                    ['name', 'Name'],
                    ['host', 'Host'],
                    ['workload', 'Workload'],
                    ['protocol', 'Protocol'],
                  ]"
                  :key="column"
                  :name="column!"
                  :sort-key="portSort.key.value"
                  :descending="portSort.descending.value"
                  @sort="portSort.toggle"
                  >{{ label }}</SortHeader
                >
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="port in portSort.sorted.value"
                :key="`${port.to}-${port.protocol}`"
                class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
              >
                <td class="px-4 py-2.5">{{ port.name || "—" }}</td>
                <td class="px-4 py-2.5 font-mono text-xs">
                  <a
                    v-if="port.protocol === 'tcp'"
                    :href="`http://${host}:${port.from}`"
                    target="_blank"
                    rel="noopener"
                    class="text-pulse-700 dark:text-pulse-300 hover:underline"
                  >
                    {{ port.from }}
                  </a>
                  <template v-else>{{ port.from }}</template>
                </td>
                <td class="px-4 py-2.5 font-mono text-xs">{{ port.to }}</td>
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ port.protocol }}
                </td>
              </tr>
            </tbody>
          </table>
        </DetailCard>
      </div>

      <div class="mt-6">
        <DetailCard title="Logs">
          <LogViewer :workload="name" :instance="index" />
        </DetailCard>
      </div>
    </template>
  </DetailPage>
</template>
