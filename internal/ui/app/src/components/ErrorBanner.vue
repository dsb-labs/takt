<script setup lang="ts">
defineProps<{ message: string }>();
</script>

<template>
  <div
    class="mt-4 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
  >
    {{ message }}
  </div>
</template>
