<script setup lang="ts">
import { ref } from "vue";

import ErrorBanner from "./ErrorBanner.vue";

// The delete flow every resource shares: a button that opens a modal with a
// force checkbox. A plain delete of something another workload reads is
// refused by the server, and the refusal stays in the modal with its reason so
// the force checkbox can be ticked and the delete sent again.
const props = defineProps<{
  subject: string;
  remove: (force: boolean) => Promise<unknown>;
}>();

const emit = defineEmits<{ deleted: [] }>();

const open = ref(false);
const force = ref(false);
const error = ref("");
const busy = ref(false);

function show() {
  open.value = true;
  force.value = false;
  error.value = "";
}

async function run() {
  error.value = "";
  busy.value = true;

  try {
    await props.remove(force.value);
    open.value = false;
    emit("deleted");
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <button
    class="rounded-md border border-rose-300 px-3 py-1.5 text-sm text-rose-700 hover:bg-rose-50 dark:border-rose-900 dark:text-rose-400 dark:hover:bg-rose-950"
    @click="show"
  >
    Delete
  </button>

  <Teleport to="body">
    <div
      v-if="open"
      class="fixed inset-0 z-20 flex items-center justify-center bg-black/50 p-4"
      @click.self="open = false"
    >
      <div
        class="w-full max-w-md rounded-lg border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900"
      >
        <h2 class="text-lg font-semibold">Delete {{ subject }}?</h2>

        <ErrorBanner v-if="error" :message="error" />

        <label class="mt-4 flex items-start gap-2 text-sm">
          <input
            v-model="force"
            type="checkbox"
            class="mt-0.5 accent-rose-600"
          />
          <span>
            Force the delete.
            <span class="text-slate-500 dark:text-slate-400">
              Deletes even while workloads still reference it.
            </span>
          </span>
        </label>

        <div class="mt-6 flex justify-end gap-2 text-sm">
          <button
            class="rounded-md border border-slate-300 px-3 py-1.5 dark:border-slate-700"
            @click="open = false"
          >
            Cancel
          </button>
          <button
            :disabled="busy"
            class="rounded-md bg-rose-700 px-3 py-1.5 font-medium text-white hover:bg-rose-800 disabled:opacity-50"
            @click="run"
          >
            Delete
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
