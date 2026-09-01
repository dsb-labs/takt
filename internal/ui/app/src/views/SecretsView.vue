<script setup lang="ts">
import { ref } from "vue";

import { useSetSecret, useDeleteSecret } from "../api/mutations";
import { useSecrets } from "../api/queries";
import ErrorBanner from "../components/ErrorBanner.vue";
import UsedByLinks from "../components/UsedByLinks.vue";
import { absoluteTime, relativeTime } from "../format";

const secrets = useSecrets();
const setSecret = useSetSecret();
const deleteSecret = useDeleteSecret();

const creating = ref(false);
const newName = ref("");
const newValue = ref("");
const rotating = ref("");
const rotateValue = ref("");
const error = ref("");

async function save(name: string, value: string) {
  error.value = "";
  try {
    await setSecret.mutateAsync({ name, value });
    creating.value = false;
    rotating.value = "";
    newName.value = "";
    newValue.value = "";
    rotateValue.value = "";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}

async function remove(name: string) {
  if (!confirm(`Delete the secret "${name}"?`)) return;
  error.value = "";
  try {
    await deleteSecret.mutateAsync(name);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between">
      <div>
        <h1 class="text-xl font-semibold">Secrets</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Values the server stores encrypted. Nothing reads one back out — only
          a starting workload sees it.
        </p>
      </div>
      <button
        class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
        @click="creating = !creating"
      >
        Set a secret
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
          type="password"
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
            <th class="px-4 py-3 font-medium">Revision</th>
            <th class="px-4 py-3 font-medium">Updated</th>
            <th class="px-4 py-3 font-medium">Used by</th>
            <th class="px-4 py-3"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="secrets.data.value?.length === 0">
            <td
              colspan="5"
              class="px-4 py-8 text-center text-slate-500 dark:text-slate-400"
            >
              No secrets.
            </td>
          </tr>
          <template v-for="secret in secrets.data.value" :key="secret.name">
            <tr
              class="border-b border-slate-100 last:border-b-0 dark:border-slate-800/50"
            >
              <td class="px-4 py-3 font-medium">{{ secret.name }}</td>
              <td
                class="px-4 py-3 font-mono text-xs text-slate-500 dark:text-slate-400"
              >
                {{ secret.revision }}
              </td>
              <td
                class="px-4 py-3 text-slate-600 dark:text-slate-400"
                :title="absoluteTime(secret.updatedAt)"
              >
                {{ relativeTime(secret.updatedAt) }}
              </td>
              <td class="px-4 py-3">
                <UsedByLinks :used-by="secret.usedBy" />
              </td>
              <td class="px-4 py-3 text-right whitespace-nowrap">
                <button
                  class="text-ocean-700 dark:text-ocean-300 hover:underline"
                  @click="
                    rotating = rotating === secret.name ? '' : secret.name
                  "
                >
                  Rotate
                </button>
                <button
                  class="ml-3 text-rose-700 hover:underline dark:text-rose-400"
                  @click="remove(secret.name)"
                >
                  Delete
                </button>
              </td>
            </tr>
            <tr
              v-if="rotating === secret.name"
              class="border-b border-slate-100 dark:border-slate-800/50"
            >
              <td colspan="5" class="px-4 py-3">
                <form
                  class="flex flex-wrap items-center gap-3"
                  @submit.prevent="save(secret.name, rotateValue)"
                >
                  <input
                    v-model="rotateValue"
                    type="password"
                    placeholder="New value"
                    class="min-w-64 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
                  />
                  <button
                    type="submit"
                    class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white"
                  >
                    Rotate
                  </button>
                  <button
                    type="button"
                    class="rounded-md border border-slate-300 px-3 py-1.5 text-sm dark:border-slate-700"
                    @click="rotating = ''"
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
