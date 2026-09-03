<script setup lang="ts">
import DetailCard from "./DetailCard.vue";
import { labelQuery } from "../filter";

// The labels on a resource, each linking to its list filtered by that label.
// The target names the list page the resource belongs to.
defineProps<{ labels?: Record<string, string>; target: string }>();
</script>

<template>
  <DetailCard title="Labels">
    <p
      v-if="!labels || Object.keys(labels).length === 0"
      class="px-4 py-6 text-sm text-slate-500 dark:text-slate-400"
    >
      No labels.
    </p>
    <div v-else class="flex flex-wrap gap-2 px-4 py-3">
      <RouterLink
        v-for="(value, key) in labels"
        :key="key"
        :to="{ path: target, query: { query: labelQuery(key, value) } }"
        class="hover:border-ocean-300 hover:text-ocean-700 dark:hover:border-ocean-700 dark:hover:text-ocean-300 rounded-md border border-slate-200 bg-slate-50 px-2.5 py-1 font-mono text-xs text-slate-700 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300"
        :title="`Show everything labelled ${key}=${value}`"
      >
        {{ key }}={{ value }}
      </RouterLink>
    </div>
  </DetailCard>
</template>
