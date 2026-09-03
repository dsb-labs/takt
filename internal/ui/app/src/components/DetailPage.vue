<script setup lang="ts">
import ErrorBanner from "./ErrorBanner.vue";

// The frame every detail page shares: the breadcrumb back to its list, the
// error banner when the resource cannot be read, and the header row holding
// the name, any badges, and the page's actions.
defineProps<{
  section: string;
  sectionTo: string;
  name: string;
  error?: string;
}>();
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink :to="sectionTo" class="hover:underline">{{
        section
      }}</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <ErrorBanner v-if="error" :message="error" />

    <template v-else>
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="text-xl font-semibold">{{ name }}</h1>
        <slot name="header" />
        <div class="ml-auto flex gap-2 text-sm">
          <slot name="actions" />
        </div>
      </header>

      <slot />
    </template>
  </div>
</template>
