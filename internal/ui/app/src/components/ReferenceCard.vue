<script setup lang="ts">
import DetailCard from "./DetailCard.vue";
import SortHeader from "./SortHeader.vue";
import { referenceTarget, type Reference } from "../references";
import { useSort } from "../sort";

// One kind of reference a workload reads, as its own card. Renders nothing
// when the workload reads none of this kind.
const props = defineProps<{ title: string; refs: Reference[] }>();

const sort = useSort(() => props.refs, "name", {
  name: (ref) => ref.name,
  via: (ref) => ref.via,
});
</script>

<template>
  <DetailCard v-if="refs.length > 0" :title="title">
    <div class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
          >
            <SortHeader
              name="name"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Name</SortHeader
            >
            <SortHeader
              name="via"
              :sort-key="sort.key.value"
              :descending="sort.descending.value"
              @sort="sort.toggle"
              >Via</SortHeader
            >
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="reference in sort.sorted.value"
            :key="`${reference.name}-${reference.via}`"
            class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
          >
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
    </div>
  </DetailCard>
</template>
