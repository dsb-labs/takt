<script setup lang="ts">
import DetailCard from "./DetailCard.vue";
import SortHeader from "./SortHeader.vue";
import { useSort } from "../sort";

// The workloads reading a resource, as a sortable table. A resource nothing
// uses says so, since that is the one that can be deleted without forcing.
const props = defineProps<{ usedBy?: string[] }>();

const sort = useSort(() => props.usedBy, "workload", {
  workload: (name) => name,
});
</script>

<template>
  <DetailCard title="Used by">
    <p
      v-if="!usedBy?.length"
      class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
    >
      Unused.
    </p>
    <div v-else class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
          >
            <SortHeader
              name="workload"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Workload</SortHeader
            >
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="workload in sort.sorted.value"
            :key="workload"
            class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
          >
            <td class="px-4 py-2.5 font-medium">
              <RouterLink
                :to="`/workloads/${workload}`"
                class="text-pulse-700 dark:text-pulse-300 hover:underline"
              >
                {{ workload }}
              </RouterLink>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </DetailCard>
</template>
