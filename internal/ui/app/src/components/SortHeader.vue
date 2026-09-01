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
      <!-- Always occupies its width, so choosing a column does not resize
           the table under the pointer. -->
      <span class="w-3" :class="{ invisible: sortKey !== name }">
        {{ descending ? "▼" : "▲" }}
      </span>
    </button>
  </th>
</template>
