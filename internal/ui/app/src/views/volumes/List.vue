<script setup lang="ts">
import { useVolumes } from "../../api/queries";
import { absoluteTime, relativeTime } from "../../format";

const volumes = useVolumes();
</script>

<template>
  <div>
    <h1 class="text-xl font-semibold">Volumes</h1>
    <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
      Directories that outlive the workloads mounting them. Creation stays in
      the CLI, where the manifest lives.
    </p>

    <div
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Created</th>
            <th class="px-4 py-3 font-medium">Used by</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="volumes.data.value?.length === 0">
            <td
              colspan="3"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No volumes. Apply a volume manifest with the CLI to create one.
            </td>
          </tr>
          <tr
            v-for="volume in volumes.data.value"
            :key="volume.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/volumes/${volume.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
              >
                {{ volume.name }}
              </RouterLink>
            </td>
            <td
              class="px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="absoluteTime(volume.createdAt)"
            >
              {{ relativeTime(volume.createdAt) }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{
                volume.usedBy?.length
                  ? `${volume.usedBy.length} workloads`
                  : "unused"
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
