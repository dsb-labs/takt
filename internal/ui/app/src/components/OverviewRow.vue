<script setup lang="ts">
// One labelled row of a detail card's description list. Mono is for a value
// pasted into a terminal, such as an image or a path.
defineProps<{ label: string; mono?: boolean; title?: string }>();
</script>

<template>
  <div class="flex gap-4 px-4 py-2.5">
    <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
      {{ label }}
    </dt>
    <dd
      :class="mono ? 'font-mono text-xs leading-5 break-all' : ''"
      :title="title"
    >
      <slot />
    </dd>
  </div>
</template>
