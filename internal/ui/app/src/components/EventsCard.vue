<script setup lang="ts">
import DetailCard from "@/components/DetailCard.vue";
import Tooltip from "@/components/Tooltip.vue";
import { absoluteTime, relativeTime } from "@/lib/format";
import type { WorkloadEvent } from "@/api/types";

// What the server observed about a workload while converging it, most recently
// seen first. This answers why the workload looks the way it does, where the
// state badge says only what it looks like.
//
// The order is the server's, so nothing sorts here. A workload with no events
// says so rather than showing an empty table: on a workload that has just been
// applied, nothing recorded yet is the ordinary case.
defineProps<{ events?: WorkloadEvent[] }>();
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
            <th class="hidden px-4 py-2 font-medium sm:table-cell">Reason</th>
            <th class="px-4 py-2 font-medium">Last seen</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="event in events"
            :key="`${event.reason}-${event.firstSeen}`"
            class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
          >
            <td class="px-4 py-2.5">
              {{ event.message }}
              <Tooltip
                v-if="event.count > 1"
                :text="`seen ${event.count} times since ${absoluteTime(event.firstSeen)}`"
              >
                <span
                  class="ml-1.5 rounded bg-slate-100 px-1.5 py-0.5 text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-300"
                >
                  &times;{{ event.count }}
                </span>
              </Tooltip>
            </td>
            <td
              class="hidden px-4 py-2.5 font-mono text-xs text-slate-500 sm:table-cell dark:text-slate-400"
            >
              {{ event.reason }}
            </td>
            <td
              class="px-4 py-2.5 text-slate-500 dark:text-slate-400"
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
