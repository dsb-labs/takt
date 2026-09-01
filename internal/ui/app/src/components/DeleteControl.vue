<script setup lang="ts">
import { ref } from "vue";

import ErrorBanner from "./ErrorBanner.vue";

// The delete flow every resource shares. A plain delete is refused by the
// server when something still reads the resource, so a refusal shows the
// reason and offers to force it — the same escalation the CLI offers.
const props = defineProps<{
  subject: string;
  remove: (force: boolean) => Promise<unknown>;
}>();

const emit = defineEmits<{ deleted: [] }>();

const error = ref("");
const refused = ref(false);

async function run(force: boolean) {
  const question = force
    ? `Force the deletion of ${props.subject}? Whatever reads it loses it.`
    : `Delete ${props.subject}?`;
  if (!confirm(question)) return;

  error.value = "";
  refused.value = false;

  try {
    await props.remove(force);
    emit("deleted");
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
    refused.value = !force;
  }
}
</script>

<template>
  <div>
    <button
      class="rounded-md border border-rose-300 px-3 py-1.5 text-sm text-rose-700 hover:bg-rose-50 dark:border-rose-900 dark:text-rose-400 dark:hover:bg-rose-950"
      @click="run(false)"
    >
      Delete
    </button>

    <ErrorBanner v-if="error" :message="error" />

    <button
      v-if="refused"
      class="mt-3 rounded-md bg-rose-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-rose-800"
      @click="run(true)"
    >
      Delete anyway
    </button>
  </div>
</template>
