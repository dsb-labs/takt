<script setup lang="ts">
// A hover tooltip that works over a disabled control: the wrapper receives the
// hover the disabled element cannot.
defineProps<{ text?: string }>();
</script>

<template>
  <span class="group relative inline-flex">
    <slot />
    <span
      v-if="text"
      class="pointer-events-none absolute bottom-full left-1/2 z-10 mb-1.5 hidden w-max max-w-64 -translate-x-1/2 rounded-md bg-slate-900 px-2.5 py-1.5 text-xs text-white group-hover:block dark:bg-slate-700"
    >
      {{ text }}
    </span>
  </span>
</template>
