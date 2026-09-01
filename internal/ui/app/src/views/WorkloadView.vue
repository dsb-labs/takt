<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteWorkload, useWorkloadAction } from "../api/mutations";
import { useWorkload } from "../api/queries";
import DeleteControl from "../components/DeleteControl.vue";
import DetailCard from "../components/DetailCard.vue";
import ErrorBanner from "../components/ErrorBanner.vue";
import LogViewer from "../components/LogViewer.vue";
import StateBadge from "../components/StateBadge.vue";
import Tooltip from "../components/Tooltip.vue";
import { absoluteTime, relativeTime } from "../format";
import { referenceTarget, references } from "../references";

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

function healthLabel(instance: {
  health?: { status: string; failures?: number };
}): string {
  if (!instance.health) return "—";
  const failures = instance.health.failures;
  return failures
    ? `${instance.health.status} (${failures} failed)`
    : instance.health.status;
}
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
            <div v-if="spec?.schedule" class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Schedule
              </dt>
              <dd class="font-mono text-xs leading-5">
                {{ spec.schedule.cron }}
              </dd>
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
                v-for="(ref, index) in refs"
                :key="index"
                class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
              >
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ ref.kind }}
                </td>
                <td class="px-4 py-2.5 font-medium">
                  <RouterLink
                    :to="referenceTarget(ref)"
                    class="text-ocean-700 dark:text-ocean-300 hover:underline"
                  >
                    {{ ref.name }}
                  </RouterLink>
                </td>
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ ref.via }}
                </td>
              </tr>
            </tbody>
          </table>
        </DetailCard>
      </div>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Instances">
          <p
            v-if="!workload.data.value.instances?.length"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            Nothing is running.
          </p>
          <table v-else class="w-full text-left text-sm">
            <thead>
              <tr
                class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
              >
                <th class="px-4 py-2 font-medium">Index</th>
                <th class="px-4 py-2 font-medium">State</th>
                <th class="px-4 py-2 font-medium">Health</th>
                <th class="px-4 py-2 font-medium">Started</th>
                <th class="px-4 py-2 font-medium">Exit code</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="instance in workload.data.value.instances"
                :key="instance.id"
                class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
              >
                <td class="px-4 py-2.5">{{ instance.index ?? 0 }}</td>
                <td class="px-4 py-2.5">{{ instance.state }}</td>
                <td class="px-4 py-2.5" :title="instance.health?.error">
                  {{ healthLabel(instance) }}
                </td>
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{
                    instance.startedAt ? relativeTime(instance.startedAt) : "—"
                  }}
                </td>
                <td class="px-4 py-2.5 text-slate-500 dark:text-slate-400">
                  {{ instance.exitCode ?? "—" }}
                </td>
              </tr>
            </tbody>
          </table>
        </DetailCard>

        <DetailCard title="Ports">
          <p
            v-if="!workload.data.value.ports?.length"
            class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
          >
            This workload publishes no ports.
          </p>
          <table v-else class="w-full text-left text-sm">
            <thead>
              <tr
                class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
              >
                <th class="px-4 py-2 font-medium">Instance</th>
                <th class="px-4 py-2 font-medium">Name</th>
                <th class="px-4 py-2 font-medium">Host</th>
                <th class="px-4 py-2 font-medium">Workload</th>
                <th class="px-4 py-2 font-medium">Protocol</th>
                <th class="px-4 py-2 font-medium">Allocation</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(port, index) in workload.data.value.ports"
                :key="index"
                class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
              >
                <td class="px-4 py-2.5">{{ port.instance ?? 0 }}</td>
                <td class="px-4 py-2.5">{{ port.name || "—" }}</td>
                <td class="px-4 py-2.5 font-mono text-xs">{{ port.from }}</td>
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
        </DetailCard>
      </div>

      <div class="mt-6">
        <DetailCard title="Logs">
          <LogViewer :workload="name" :count="spec?.count ?? 1" />
        </DetailCard>
      </div>

      <div class="mt-6">
        <DeleteControl
          :subject="`the workload ${name}`"
          :remove="(force) => deletion.mutateAsync(force)"
          @deleted="router.push('/')"
        />
      </div>
    </template>
  </div>
</template>
