<script setup lang="ts">
import { useReadiness } from "./api/queries";

const readiness = useReadiness();

const navigation = [
  { name: "Workloads", to: "/" },
  { name: "Secrets", to: "/secrets" },
  { name: "Variables", to: "/variables" },
  { name: "Volumes", to: "/volumes" },
];
</script>

<template>
  <div
    class="flex min-h-screen flex-col bg-slate-50 text-slate-900 sm:flex-row dark:bg-slate-950 dark:text-slate-100"
  >
    <aside
      class="flex shrink-0 flex-col border-b border-slate-200 bg-white sm:min-h-screen sm:w-56 sm:border-r sm:border-b-0 dark:border-slate-800 dark:bg-slate-900"
    >
      <RouterLink to="/" class="flex items-center gap-2.5 px-4 py-4">
        <img src="/orca.svg" alt="" class="h-7 w-7" />
        <span class="text-lg font-semibold tracking-tight">orca</span>
      </RouterLink>

      <nav class="flex gap-1 px-2 pb-2 sm:flex-col sm:pb-0">
        <RouterLink
          v-for="item in navigation"
          :key="item.to"
          :to="item.to"
          class="rounded-md px-3 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-100"
          exact-active-class="bg-ocean-50 text-ocean-800 hover:bg-ocean-50 hover:text-ocean-800 dark:bg-ocean-950 dark:text-ocean-200 dark:hover:bg-ocean-950 dark:hover:text-ocean-200"
        >
          {{ item.name }}
        </RouterLink>
      </nav>

      <div
        class="mt-auto hidden items-center gap-2 px-4 py-4 text-xs text-slate-500 sm:flex dark:text-slate-400"
        :title="readiness.data.value?.reasons?.join(', ')"
      >
        <span
          class="h-2 w-2 rounded-full"
          :class="
            readiness.data.value?.ready ? 'bg-emerald-500' : 'bg-rose-500'
          "
        ></span>
        {{ readiness.data.value?.ready ? "server ready" : "server not ready" }}
      </div>
    </aside>

    <main class="min-w-0 flex-1 p-4 sm:p-8">
      <RouterView />
    </main>
  </div>
</template>
