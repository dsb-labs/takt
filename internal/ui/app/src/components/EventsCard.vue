<script setup lang="ts">
import DetailCard from "@/components/DetailCard.vue";
import SortHeader from "@/components/SortHeader.vue";
import { useSort } from "@/composables/sort";
import { absoluteTime, relativeTime } from "@/lib/format";
import type { WorkloadEvent } from "@/api/types";

// What the server observed about a workload while converging it. This answers
// why the workload looks the way it does, where the state badge says only what
// it looks like.
//
// A workload with no events says so rather than showing an empty table: on one
// that has just been applied, nothing recorded yet is the ordinary case.
const props = defineProps<{ events?: WorkloadEvent[] }>();

// Most recently seen first, which is the order the server returns them in and
// the one that answers what is happening now. Sorting by count instead answers
// a different question: which cause the server has hit most while converging.
//
// The reason is not a column: it names the same thing the message says, in a
// form written for a machine.
const sort = useSort(
  () => props.events,
  "lastSeen",
  {
    count: (event) => event.count,
    lastSeen: (event) => event.lastSeen,
  },
  true,
);
</script>

<template>
  <DetailCard title="Events">
    <p
      v-if="!events?.length"
      class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
    >
      Nothing recorded.
    </p>
    <div v-else class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-2 font-medium">Event</th>
            <SortHeader
              name="count"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Count</SortHeader
            >
            <SortHeader
              name="lastSeen"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Last seen</SortHeader
            >
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="event in sort.sorted.value"
            :key="`${event.reason}-${event.firstSeen}`"
            class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
          >
            <td class="px-4 py-2.5">{{ event.message }}</td>
            <td
              class="px-4 py-2.5 whitespace-nowrap text-slate-500 dark:text-slate-400"
              :title="`First seen ${absoluteTime(event.firstSeen)}`"
            >
              {{ event.count }}
            </td>
            <td
              class="px-4 py-2.5 whitespace-nowrap text-slate-500 dark:text-slate-400"
              :title="absoluteTime(event.lastSeen)"
            >
              {{ relativeTime(event.lastSeen) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </DetailCard>
</template>
