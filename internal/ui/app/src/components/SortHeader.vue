<script setup lang="ts">
defineProps<{ name: string; sortKey: string; descending: boolean }>();

const emit = defineEmits<{ sort: [name: string] }>();
</script>

<template>
  <th class="px-4 py-3 font-medium">
    <button
      class="flex items-center gap-1 uppercase hover:text-slate-900 dark:hover:text-slate-100"
      @click="emit('sort', name)"
    >
      <slot />
      <span v-if="sortKey === name">{{ descending ? "▼" : "▲" }}</span>
    </button>
  </th>
</template>
