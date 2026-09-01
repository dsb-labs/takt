<script setup lang="ts">
import { useVariables } from "../api/queries";
import { absoluteTime, relativeTime } from "../format";

const variables = useVariables();
</script>

<template>
  <div>
    <div class="flex items-center justify-between">
      <div>
        <h1 class="text-xl font-semibold">Variables</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Plain configuration values workloads read by reference.
        </p>
      </div>
      <RouterLink
        to="/variables/new"
        class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
      >
        Set a variable
      </RouterLink>
    </div>

    <div
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Updated</th>
            <th class="px-4 py-3 font-medium">Used by</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="variables.data.value?.length === 0">
            <td
              colspan="3"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No variables.
            </td>
          </tr>
          <tr
            v-for="variable in variables.data.value"
            :key="variable.name"
            class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
          >
            <td class="px-4 py-3 font-medium">
              <RouterLink
                :to="`/variables/${variable.name}`"
                class="text-ocean-700 dark:text-ocean-300 hover:underline"
              >
                {{ variable.name }}
              </RouterLink>
            </td>
            <td
              class="px-4 py-3 text-slate-600 dark:text-slate-400"
              :title="absoluteTime(variable.updatedAt)"
            >
              {{ relativeTime(variable.updatedAt) }}
            </td>
            <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
              {{
                variable.usedBy?.length
                  ? `${variable.usedBy.length} workloads`
                  : "unused"
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
