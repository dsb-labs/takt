<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteWorkload, useWorkloadAction } from "../../api/mutations";
import { useWorkload } from "../../api/queries";
import DeleteControl from "../../components/DeleteControl.vue";
import DetailCard from "../../components/DetailCard.vue";
import LabelsCard from "../../components/LabelsCard.vue";
import ErrorBanner from "../../components/ErrorBanner.vue";
import LogViewer from "../../components/LogViewer.vue";
import SortHeader from "../../components/SortHeader.vue";
import SpecView from "../../components/SpecView.vue";
import StateBadge from "../../components/StateBadge.vue";
import Tooltip from "../../components/Tooltip.vue";
import { absoluteTime, healthStyles, relativeTime } from "../../format";
import { referenceTarget, references } from "../../references";
import { useSort } from "../../sort";

const route = useRoute();
const name = computed(() => route.params.name as string);

const workload = useWorkload(() => name.value);

const router = useRouter();
const stop = useWorkloadAction("stop", () => name.value);
const start = useWorkloadAction("start", () => name.value);
const restart = useWorkloadAction("restart", () => name.value);
const deletion = useDeleteWorkload(() => name.value);
const actionError = ref("");

async function act(action: { mutateAsync: () => Promise<unknown> }) {
  actionError.value = "";
  try {
    await action.mutateAsync();
  } catch (cause) {
    actionError.value = cause instanceof Error ? cause.message : String(cause);
  }
}

const spec = computed(() => workload.data.value?.spec);
const refs = computed(() => (spec.value ? references(spec.value) : []));
const command = computed(() => {
  const runtime = spec.value?.container ?? spec.value?.exec;
  return runtime?.command?.join(" ");
});

// check lists what the health check does as rows, with the defaults the
// server applies when the specification leaves a field unset.
const check = computed(() => {
  const health = spec.value?.health;
  if (!health) return null;

  const rows: [string, string][] = [];
  if (health.http) rows.push(["Path", health.http]);
  if (health.port) rows.push(["Port", health.port]);
  rows.push(
    ["Interval", health.interval ?? "10s"],
    ["Timeout", health.timeout ?? "2s"],
    ["Retries", String(health.retries ?? 3)],
    ["Start period", health.startPeriod ?? "30s"],
  );

  return rows;
});

// The address the page was loaded from is the host the ports are published
// on, so a host port links straight to what it forwards to.
const host = window.location.hostname;

// The driver's handle for an instance — a container ID for the docker
// runtime — is what a docker logs or docker exec wants pasted.
const copiedID = ref("");

async function copyID(id: string) {
  await navigator.clipboard.writeText(id);
  copiedID.value = id;
  setTimeout(() => {
    if (copiedID.value === id) copiedID.value = "";
  }, 1500);
}

function healthLabel(instance: {
  health?: { status: string; failures?: number };
}): string {
  if (!instance.health) return "—";
  const failures = instance.health.failures;
  return failures
    ? `${instance.health.status} (${failures} failed)`
    : instance.health.status;
}

const instanceSort = useSort(() => workload.data.value?.instances, "index", {
  index: (i) => i.index ?? 0,
  id: (i) => i.id,
  state: (i) => i.state,
  health: (i) => i.health?.status ?? "",
  started: (i) => i.startedAt ?? "",
  exit: (i) => i.exitCode ?? -1,
});

const portSort = useSort(() => workload.data.value?.ports, "instance", {
  instance: (p) => p.instance ?? 0,
  name: (p) => p.name ?? "",
  host: (p) => p.from,
  workload: (p) => p.to,
  protocol: (p) => p.protocol ?? "",
  allocation: (p) => (p.dynamic ? 1 : 0),
});

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await workload.suspense().catch(() => {});
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/" class="hover:underline">Workloads</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <div
      v-if="workload.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to read the workload: {{ workload.error.value?.message }}
    </div>

    <template v-else-if="workload.data.value">
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="text-xl font-semibold">{{ workload.data.value.name }}</h1>
        <StateBadge :state="workload.data.value.state" />
        <span class="text-sm text-slate-500 dark:text-slate-400">
          {{ workload.data.value.runtime }} · version
          {{ workload.data.value.version }}
        </span>

        <div class="ml-auto flex gap-2 text-sm">
          <button
            v-if="workload.data.value.suspended"
            class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 font-medium text-white"
            @click="act(start)"
          >
            Start
          </button>
          <button
            v-else
            class="rounded-md border border-slate-300 px-3 py-1.5 hover:bg-slate-100 dark:border-slate-700 dark:hover:bg-slate-800"
            @click="act(stop)"
          >
            Stop
          </button>
          <Tooltip
            :text="
              workload.data.value.suspended
                ? 'A suspended workload has nothing to restart. Start it instead.'
                : undefined
            "
          >
            <button
              class="rounded-md border border-slate-300 px-3 py-1.5 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-700 dark:hover:bg-slate-800"
              :disabled="workload.data.value.suspended"
              @click="act(restart)"
            >
              Restart
            </button>
          </Tooltip>
          <DeleteControl
            :subject="`the workload ${name}`"
            :remove="(force) => deletion.mutateAsync(force)"
            @deleted="router.push('/')"
          />
        </div>
      </header>

      <ErrorBanner v-if="actionError" :message="actionError" />

      <div
        v-if="workload.data.value.lastError"
        class="mt-4 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
      >
        <span v-if="workload.data.value.lastErrorAt" class="font-medium">
          {{ relativeTime(workload.data.value.lastErrorAt) }}:
        </span>
        {{ workload.data.value.lastError }}
      </div>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div v-if="command" class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                {{ spec?.container ? "Image" : "Command" }}
              </dt>
              <dd class="font-mono text-xs leading-5 break-all">
                {{ spec?.container ? spec.container.image : command }}
              </dd>
            </div>
            <div v-if="spec?.container?.command" class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Command
              </dt>
              <dd class="font-mono text-xs leading-5 break-all">
                {{ spec.container.command.join(" ") }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Count
              </dt>
              <dd>{{ spec?.count ?? 1 }}</dd>
            </div>
            <div v-if="spec?.restart" class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Restart
              </dt>
              <dd>{{ spec.restart.policy }}</dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Created
              </dt>
              <dd :title="absoluteTime(workload.data.value.createdAt)">
                {{ relativeTime(workload.data.value.createdAt) }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Updated
              </dt>
              <dd :title="absoluteTime(workload.data.value.updatedAt)">
                {{ relativeTime(workload.data.value.updatedAt) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <DetailCard title="References">
          <p
            v-if="refs.length === 0"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            This workload references no secrets, variables, volumes or other
            workloads.
          </p>
          <table v-else class="w-full text-left text-sm">
            <tbody>
              <tr
                v-for="(reference, index) in refs"
                :key="index"
                class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
              >
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ reference.kind }}
                </td>
                <td class="px-4 py-2.5 font-medium">
                  <RouterLink
                    :to="referenceTarget(reference)"
                    class="text-ocean-700 dark:text-ocean-300 hover:underline"
                  >
                    {{ reference.name }}
                  </RouterLink>
                </td>
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ reference.via }}
                </td>
              </tr>
            </tbody>
          </table>
        </DetailCard>

        <DetailCard v-if="check" title="Health check">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div
              v-for="[label, value] in check"
              :key="label"
              class="flex gap-4 px-4 py-2.5"
            >
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                {{ label }}
              </dt>
              <dd :class="{ 'font-mono text-xs leading-5': label === 'Probe' }">
                {{ value }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <DetailCard v-if="spec?.schedule" title="Schedule">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Cron
              </dt>
              <dd class="font-mono text-xs leading-5">
                {{ spec.schedule.cron }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Overlap
              </dt>
              <dd>{{ spec.schedule.overlap ?? "replace" }}</dd>
            </div>
            <div
              v-if="workload.data.value.nextRun"
              class="flex gap-4 px-4 py-2.5"
            >
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Next run
              </dt>
              <dd :title="absoluteTime(workload.data.value.nextRun)">
                {{ relativeTime(workload.data.value.nextRun) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <LabelsCard :labels="spec?.labels" target="/" />
      </div>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Instances">
          <p
            v-if="!workload.data.value.instances?.length"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            Nothing is running.
          </p>
          <div v-else class="overflow-x-auto">
            <table class="w-full text-left text-sm">
              <thead>
                <tr
                  class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
                >
                  <SortHeader
                    v-for="[column, label] in [
                      ['index', 'Index'],
                      ['id', 'ID'],
                      ['state', 'State'],
                      ['health', 'Health'],
                      ['started', 'Started'],
                      ['exit', 'Exit code'],
                    ]"
                    :key="column"
                    :name="column!"
                    :sort-key="instanceSort.key.value"
                    :descending="instanceSort.descending.value"
                    @sort="instanceSort.toggle"
                    >{{ label }}</SortHeader
                  >
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="instance in instanceSort.sorted.value"
                  :key="instance.id"
                  class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
                >
                  <td class="px-4 py-2.5">{{ instance.index ?? 0 }}</td>
                  <td class="px-4 py-2.5 font-mono text-xs">
                    <span class="inline-flex items-center gap-1.5">
                      <span :title="instance.id">{{
                        instance.id.slice(0, 12)
                      }}</span>
                      <button
                        class="hover:text-ocean-700 dark:hover:text-ocean-300 text-slate-400 dark:text-slate-500"
                        title="Copy the full ID"
                        @click="copyID(instance.id)"
                      >
                        <svg
                          v-if="copiedID !== instance.id"
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
                  </td>
                  <td class="px-4 py-2.5">{{ instance.state }}</td>
                  <td
                    class="px-4 py-2.5"
                    :class="
                      instance.health && healthStyles[instance.health.status]
                    "
                    :title="instance.health?.error"
                  >
                    {{ healthLabel(instance) }}
                  </td>
                  <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                    {{
                      instance.startedAt
                        ? relativeTime(instance.startedAt)
                        : "—"
                    }}
                  </td>
                  <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                    {{ instance.exitCode ?? "—" }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </DetailCard>

        <DetailCard title="Ports">
          <p
            v-if="!workload.data.value.ports?.length"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            This workload publishes no ports.
          </p>
          <div v-else class="overflow-x-auto">
            <table class="w-full text-left text-sm">
              <thead>
                <tr
                  class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
                >
                  <SortHeader
                    v-for="[column, label] in [
                      ['instance', 'Instance'],
                      ['name', 'Name'],
                      ['host', 'Host'],
                      ['workload', 'Workload'],
                      ['protocol', 'Protocol'],
                      ['allocation', 'Allocation'],
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
                  :key="`${port.instance}-${port.to}-${port.protocol}`"
                  class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
                >
                  <td class="px-4 py-2.5">{{ port.instance ?? 0 }}</td>
                  <td class="px-4 py-2.5">{{ port.name || "—" }}</td>
                  <td class="px-4 py-2.5 font-mono text-xs">
                    <a
                      v-if="port.protocol === 'tcp'"
                      :href="`http://${host}:${port.from}`"
                      target="_blank"
                      rel="noopener"
                      class="text-ocean-700 dark:text-ocean-300 hover:underline"
                    >
                      {{ port.from }}
                    </a>
                    <template v-else>{{ port.from }}</template>
                  </td>
                  <td class="px-4 py-2.5 font-mono text-xs">{{ port.to }}</td>
                  <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                    {{ port.protocol }}
                  </td>
                  <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                    {{ port.dynamic ? "dynamic" : "pinned" }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </DetailCard>
      </div>

      <div class="mt-6">
        <DetailCard title="Logs">
          <LogViewer :workload="name" :count="spec?.count ?? 1" />
        </DetailCard>
      </div>

      <div v-if="spec" class="mt-6">
        <DetailCard title="Specification">
          <SpecView :spec="spec" />
        </DetailCard>
      </div>
    </template>
  </div>
</template>
