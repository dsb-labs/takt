<script setup lang="ts">
import { ref } from "vue";

import { useSetVariable, useDeleteVariable } from "../api/mutations";
import { useVariables } from "../api/queries";
import ErrorBanner from "../components/ErrorBanner.vue";
import UsedByLinks from "../components/UsedByLinks.vue";
import { absoluteTime, relativeTime } from "../format";

const variables = useVariables();
const setVariable = useSetVariable();
const deleteVariable = useDeleteVariable();

const creating = ref(false);
const newName = ref("");
const newValue = ref("");
const editing = ref("");
const editValue = ref("");
const error = ref("");

async function save(name: string, value: string) {
  error.value = "";
  try {
    await setVariable.mutateAsync({ name, value });
    creating.value = false;
    editing.value = "";
    newName.value = "";
    newValue.value = "";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}

function edit(name: string, value: string) {
  editing.value = editing.value === name ? "" : name;
  editValue.value = value;
}

async function remove(name: string) {
  if (!confirm(`Delete the variable "${name}"?`)) return;
  error.value = "";
  try {
    await deleteVariable.mutateAsync(name);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between">
      <div>
        <h1 class="text-xl font-semibold">Variables</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Plain configuration values workloads read by reference.
        </p>
      </div>
      <button
        class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
        @click="creating = !creating"
      >
        Set a variable
      </button>
    </div>

    <form
      v-if="creating"
      class="mt-4 flex flex-wrap items-end gap-3 rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900"
      @submit.prevent="save(newName, newValue)"
    >
      <label class="flex flex-col gap-1 text-sm">
        Name
        <input
          v-model="newName"
          required
          class="rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
        />
      </label>
      <label class="flex min-w-64 flex-1 flex-col gap-1 text-sm">
        Value
        <input
          v-model="newValue"
          class="rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
        />
      </label>
      <button
        type="submit"
        class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
      >
        Save
      </button>
      <button
        type="button"
        class="rounded-md border border-slate-300 px-3 py-1.5 text-sm dark:border-slate-700"
        @click="creating = false"
      >
        Cancel
      </button>
    </form>

    <ErrorBanner v-if="error" :message="error" />

    <div
      class="mt-6 overflow-x-auto rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
    >
      <table class="w-full text-left text-sm">
        <thead>
          <tr
            class="border-b border-slate-200 text-xs text-slate-500 uppercase dark:border-slate-800 dark:text-slate-400"
          >
            <th class="px-4 py-3 font-medium">Name</th>
            <th class="px-4 py-3 font-medium">Value</th>
            <th class="px-4 py-3 font-medium">Updated</th>
            <th class="px-4 py-3 font-medium">Used by</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="variables.data.value?.length === 0">
            <td
              colspan="5"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No variables.
            </td>
          </tr>
          <template
            v-for="variable in variables.data.value"
            :key="variable.name"
          >
            <tr
              class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
            >
              <td class="px-4 py-3 font-medium">{{ variable.name }}</td>
              <td
                class="max-w-md truncate px-4 py-3 font-mono text-xs"
                :title="variable.value"
              >
                {{ variable.value }}
              </td>
              <td
                class="px-4 py-3 text-slate-600 dark:text-slate-400"
                :title="absoluteTime(variable.updatedAt)"
              >
                {{ relativeTime(variable.updatedAt) }}
              </td>
              <td class="px-4 py-3">
                <UsedByLinks :used-by="variable.usedBy" />
              </td>
              <td class="px-4 py-3 text-right whitespace-nowrap">
                <button
                  class="text-ocean-700 dark:text-ocean-300 hover:underline"
                  @click="edit(variable.name, variable.value)"
                >
                  Edit
                </button>
                <button
                  class="ml-3 text-rose-700 hover:underline dark:text-rose-400"
                  @click="remove(variable.name)"
                >
                  Delete
                </button>
              </td>
            </tr>
            <tr
              v-if="editing === variable.name"
              class="border-b border-slate-100 dark:border-slate-800/50"
            >
              <td colspan="5" class="px-4 py-3">
                <form
                  class="flex flex-wrap items-center gap-3"
                  @submit.prevent="save(variable.name, editValue)"
                >
                  <input
                    v-model="editValue"
                    class="min-w-64 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
                  />
                  <button
                    type="submit"
                    class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
                  >
                    Save
                  </button>
                  <button
                    type="button"
                    class="rounded-md border border-slate-300 px-3 py-1.5 text-sm dark:border-slate-700"
                    @click="editing = ''"
                  >
                    Cancel
                  </button>
                </form>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
  </div>
</template>
