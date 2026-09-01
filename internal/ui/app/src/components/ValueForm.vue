<script setup lang="ts">
import { ref } from "vue";

// A form for a value that may be an entire pasted file, so the input is a
// textarea rather than a line. The surrounding card names what the value is,
// so the textarea carries no label of its own — the placeholder says what to
// put in it. When withName is set the form also asks for the name, which is
// what creating needs and editing does not.
const props = defineProps<{
  submitLabel: string;
  placeholder?: string;
  withName?: boolean;
  initialValue?: string;
  busy?: boolean;
}>();

const emit = defineEmits<{ submit: [name: string, value: string] }>();

const name = ref("");
const value = ref(props.initialValue ?? "");
</script>

<template>
  <form
    class="flex flex-col gap-3"
    @submit.prevent="emit('submit', name, value)"
  >
    <label v-if="withName" class="flex flex-col gap-1 text-sm">
      Name
      <input
        v-model="name"
        required
        pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?"
        class="max-w-md rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
      />
    </label>

    <textarea
      v-model="value"
      rows="8"
      aria-label="Value"
      :placeholder="placeholder"
      class="rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800"
    ></textarea>

    <div>
      <button
        type="submit"
        :disabled="busy"
        class="bg-ocean-600 hover:bg-ocean-700 rounded-md px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
      >
        {{ submitLabel }}
      </button>
    </div>
  </form>
</template>
