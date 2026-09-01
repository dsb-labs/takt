<script setup lang="ts">
defineProps<{ title: string }>();
</script>

<template>
  <div>
    <h1 class="text-xl font-semibold">{{ title }}</h1>
    <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
      This view arrives in a later commit.
    </p>
  </div>
</template>
