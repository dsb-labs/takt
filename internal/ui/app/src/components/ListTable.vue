<script setup lang="ts" generic="T">
import SortHeader from "@/components/SortHeader.vue";
import type { Sort } from "@/composables/sort";

// The table every list view renders: a bordered shell, sortable headers, an
// empty-state row, and one hoverable row per item. The cells stay with the
// view, which passes them through the row slot. A column's class is how a
// view hides it at narrow widths, and the matching cells carry the same
// class themselves.
//
// The empty row can carry a hint naming the CLI command that creates the
// resource. Applying a specification stays in the CLI, so an empty page is
// where a reader would otherwise go looking for a button that is not there.
defineProps<{
  columns: { name: string; label: string; class?: string }[];
  sort: Sort<T>;
  rowKey: (item: T) => string;
  empty: string;
  hint?: string;
  command?: string;
}>();
</script>

<template>
  <div
    class="mt-6 scrollbar-none overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
  >
    <table class="w-full text-left text-sm">
      <thead>
        <tr
          class="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
        >
          <SortHeader
            v-for="column in columns"
            :key="column.name"
            :name="column.name"
            :class="column.class"
            :sort-key="sort.key.value"
            :descending="sort.descending.value"
            @sort="sort.toggle"
            >{{ column.label }}</SortHeader
          >
        </tr>
      </thead>
      <tbody>
        <tr v-if="sort.sorted.value.length === 0">
          <td
            :colspan="columns.length"
            class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
          >
            {{ empty }}
            <template v-if="hint && command">
              {{ hint }}
              <code
                class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs text-slate-700 dark:bg-slate-800 dark:text-slate-300"
                >{{ command }}</code
              >.
            </template>
          </td>
        </tr>
        <tr
          v-for="item in sort.sorted.value"
          :key="rowKey(item)"
          class="border-b border-slate-100 last:border-b-0 hover:bg-slate-50 dark:border-slate-800/50 dark:hover:bg-slate-800/50"
        >
          <slot name="row" :item="item" />
        </tr>
      </tbody>
    </table>
  </div>
</template>
