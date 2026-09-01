<script setup lang="ts">
defineProps<{ title: string }>();
</script>

<template>
  <section
    class="rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
  >
    <h2
      class="border-b border-slate-200 px-4 py-3 text-xs font-medium tracking-wide text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
    >
      {{ title }}
    </h2>
    <slot />
  </section>
</template>
