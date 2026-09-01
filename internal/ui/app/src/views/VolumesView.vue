<script setup lang="ts">
import { ref } from "vue";

import { useDeleteVolume } from "../api/mutations";
import { useVolumes } from "../api/queries";
import ErrorBanner from "../components/ErrorBanner.vue";
import UsedByLinks from "../components/UsedByLinks.vue";
import { absoluteTime, relativeTime } from "../format";

const volumes = useVolumes();
const deleteVolume = useDeleteVolume();

const error = ref("");

async function remove(name: string) {
  if (!confirm(`Delete the volume "${name}" and its data?`)) return;
  error.value = "";
  try {
    await deleteVolume.mutateAsync(name);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <h1 class="text-xl font-semibold">Volumes</h1>
    <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
      Directories that outlive the workloads mounting them. Creation stays in
      the CLI, where the manifest lives.
    </p>

    <ErrorBanner v-if="error" :message="error" />

    <div
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Path</th>
            <th class="px-4 py-3 font-medium">Created</th>
            <th class="px-4 py-3 font-medium">Used by</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="volumes.data.value?.length === 0">
            <td
              colspan="5"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No volumes. Apply a volume manifest with the CLI to create one.
            </td>
          </tr>
          <tr
            v-for="volume in volumes.data.value"
            :key="volume.name"
            class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">{{ volume.name }}</td>
            <td
              class="max-w-md truncate px-4 py-3 font-mono text-xs"
              :title="volume.path"
            >
              {{ volume.path }}
            </td>
            <td
              class="px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="absoluteTime(volume.createdAt)"
            >
              {{ relativeTime(volume.createdAt) }}
            </td>
            <td class="px-4 py-3"><UsedByLinks :used-by="volume.usedBy" /></td>
            <td class="px-4 py-3 text-right">
              <button
                class="text-rose-700 hover:underline disabled:cursor-not-allowed disabled:opacity-50 dark:text-rose-400"
                :disabled="(volume.usedBy?.length ?? 0) > 0"
                :title="
                  volume.usedBy?.length
                    ? 'Workloads still mount this volume, so it cannot be deleted.'
                    : undefined
                "
                @click="remove(volume.name)"
              >
                Delete
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
