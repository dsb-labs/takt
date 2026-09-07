<script setup lang="ts">
// A filter over a list, in the server's own query syntax: path=value pairs
// against the specification, separated by spaces. The list view splits the
// input and sends each pair as one query parameter.
const filter = defineModel<string>({ required: true });
</script>

<template>
  <input
    v-model="filter"
    type="search"
    placeholder="$.labels.app=web"
    class="w-full min-w-0 rounded-md border border-slate-300 bg-white px-3 py-1.5 font-mono text-sm placeholder:text-slate-400 sm:w-72 dark:border-slate-700 dark:bg-slate-800 dark:placeholder:text-slate-500"
  />
</template>
