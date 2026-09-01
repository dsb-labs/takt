<script setup lang="ts">
import { useRoute } from "vue-router";

import { version } from "../package.json";
import { useReadiness } from "./api/queries";

const route = useRoute();
const readiness = useReadiness();

// active reports whether a section owns the current page, so a detail page
// keeps its section highlighted. Workload pages live under the root.
function active(to: string): boolean {
  if (to === "/") {
    return route.path === "/" || route.path.startsWith("/workloads");
  }
  return route.path.startsWith(to);
}

// The version goreleaser wrote into package.json at release time. A build from
// the repository still carries the placeholder, which reads better as "dev".
const displayVersion = version === "0.0.0" ? "dev" : `v${version}`;

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
        <span class="mt-0.5 text-xs text-slate-400 dark:text-slate-500">{{
          displayVersion
        }}</span>
      </RouterLink>

      <nav
        class="flex divide-slate-100 border-y border-slate-100 sm:flex-col sm:divide-y dark:divide-slate-800 dark:border-slate-800"
      >
        <RouterLink
          v-for="item in navigation"
          :key="item.to"
          :to="item.to"
          class="px-4 py-2.5 text-sm font-medium"
          :class="
            active(item.to)
              ? 'bg-ocean-50 text-ocean-800 dark:bg-ocean-950 dark:text-ocean-200'
              : 'text-slate-600 hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-100'
          "
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
