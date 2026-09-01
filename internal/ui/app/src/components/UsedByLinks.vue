<script setup lang="ts">
defineProps<{ usedBy?: string[] }>();
</script>

<template>
  <span v-if="!usedBy?.length" class="text-slate-400 dark:text-slate-500"
    >unused</span
  >
  <span v-else class="flex flex-wrap gap-x-2">
    <RouterLink
      v-for="workload in usedBy"
      :key="workload"
      :to="`/workloads/${workload}`"
      class="text-ocean-700 dark:text-ocean-300 hover:underline"
    >
      {{ workload }}
    </RouterLink>
  </span>
</template>
