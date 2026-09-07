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
      <!-- On a phone the actions take a full row beneath the name and its
           badges, the way the list views lay their controls out. On a wider
           screen they sit at the right of the same row. -->
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="min-w-0 text-xl font-semibold break-all">{{ name }}</h1>
        <slot name="header" />
        <div class="flex w-full flex-wrap gap-2 text-sm sm:ml-auto sm:w-auto">
          <slot name="actions" />
        </div>
      </header>

      <slot />
    </template>
  </div>
</template>
