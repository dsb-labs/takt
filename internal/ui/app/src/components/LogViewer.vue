<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from "vue";

import Tooltip from "./Tooltip.vue";

const props = defineProps<{ workload: string; count: number }>();

const tail = ref(100);
const instance = ref<"all" | number>("all");
const previous = ref(false);
const follow = ref(false);
const text = ref("");
const error = ref("");
const loading = ref(false);
const output = ref<HTMLElement>();

const instances = computed(() =>
  Array.from({ length: props.count }, (_, index) => index),
);

// A follow reads one stream until it ends, so a workload running more than
// one instance has to say which. The server refuses the combination, and the
// control is disabled for the same reason rather than surfacing that error.
const followDisabled = computed(
  () => previous.value || (props.count > 1 && instance.value === "all"),
);

let controller: AbortController | undefined;

function params(): string {
  const query = new URLSearchParams();
  query.set("tail", String(tail.value));
  if (instance.value !== "all") query.set("instance", String(instance.value));
  if (previous.value) query.set("previous", "true");
  if (follow.value) query.set("follow", "true");
  return query.toString();
}

async function scrollToEnd() {
  await nextTick();
  output.value?.scrollTo({ top: output.value.scrollHeight });
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

// read fetches the logs once and appends what arrives. A plain fetch rather
// than the typed client: the response is a stream of text, not a JSON body to
// decode.
async function read(signal: AbortSignal) {
  const response = await fetch(
    `/api/v1/workloads/${props.workload}/logs?${params()}`,
    { signal },
  );
  if (!response.ok) {
    const body = (await response.json()) as { error?: string };
    throw new Error(body.error ?? `the server answered ${response.status}`);
  }

  const reader = response.body?.getReader();
  if (!reader) throw new Error("the response carries no body");

  const decoder = new TextDecoder();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;
    text.value += decoder.decode(value, { stream: true });
    await scrollToEnd();
  }
}

// load reads the logs, and while following keeps reading. A followed stream
// ends when its instance ends — a restart replaces the instance — so the loop
// marks the break, waits, and asks again rather than going quiet.
async function load() {
  controller?.abort();
  const mine = new AbortController();
  controller = mine;

  text.value = "";
  error.value = "";

  for (;;) {
    loading.value = true;
    try {
      await read(mine.signal);
    } catch (cause) {
      if (mine.signal.aborted) return;

      // While following, a refusal is transient: the replacement instance may
      // not exist yet. The loop retries instead of reporting it.
      if (!follow.value) {
        error.value = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      loading.value = false;
    }

    if (!follow.value || mine.signal.aborted) return;

    text.value += "\n--- the instance ended, waiting for its replacement ---\n";
    await scrollToEnd();
    await sleep(2000, mine.signal);
    if (mine.signal.aborted) return;
  }
}

watch(followDisabled, (disabled) => {
  if (disabled) follow.value = false;
});

watch([tail, instance, previous, follow], () => void load());

void load();

onUnmounted(() => controller?.abort());
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
      <label
        class="flex items-center gap-1.5 text-slate-600 dark:text-slate-400"
      >
        Instance
        <select
          v-model="instance"
          class="rounded-md border border-slate-300 bg-white px-2 py-1 dark:border-slate-700 dark:bg-slate-800"
        >
          <option value="all">all</option>
          <option v-for="index in instances" :key="index" :value="index">
            {{ index }}
          </option>
        </select>
      </label>

      <label
        class="flex items-center gap-1.5 text-slate-600 dark:text-slate-400"
      >
        Tail
        <select
          v-model.number="tail"
          class="rounded-md border border-slate-300 bg-white px-2 py-1 dark:border-slate-700 dark:bg-slate-800"
        >
          <option :value="100">100</option>
          <option :value="500">500</option>
          <option :value="1000">1000</option>
          <option :value="10000">10000</option>
        </select>
      </label>

      <label
        class="flex items-center gap-1.5 text-slate-600 dark:text-slate-400"
      >
        <input v-model="previous" type="checkbox" class="accent-ocean-600" />
        Previous
      </label>

      <Tooltip
        :text="
          followDisabled
            ? 'A follow reads one live instance, so pick an instance and turn previous off.'
            : undefined
        "
      >
        <label
          class="flex items-center gap-1.5 text-slate-600 dark:text-slate-400"
          :class="{ 'opacity-50': followDisabled }"
        >
          <input
            v-model="follow"
            type="checkbox"
            :disabled="followDisabled"
            class="accent-ocean-600"
          />
          Follow
        </label>
      </Tooltip>

      <button
        v-if="!follow"
        class="ml-auto rounded-md border border-slate-300 px-2.5 py-1 text-slate-600 hover:bg-slate-100 dark:border-slate-700 dark:text-slate-400 dark:hover:bg-slate-800"
        @click="load"
      >
        Refresh
      </button>
      <span
        v-else
        class="ml-auto flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400"
      >
        <span class="h-2 w-2 animate-pulse rounded-full bg-emerald-500"></span>
        following
      </span>
    </div>

    <div
      v-if="error"
      class="border-t border-slate-200 px-4 py-3 text-sm text-rose-700 dark:border-slate-800 dark:text-rose-400"
    >
      Failed to read logs: {{ error }}
    </div>

    <pre
      v-else
      ref="output"
      class="max-h-96 overflow-auto border-t border-slate-200 bg-slate-950 px-4 py-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-slate-200 dark:border-slate-800"
      >{{ text || (loading ? "Loading…" : "No output.") }}</pre>
  </div>
</template>
