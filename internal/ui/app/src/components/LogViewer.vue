<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from "vue";

import type { InstanceState } from "@/api/types";
import Tooltip from "@/components/Tooltip.vue";
import { parser, type Span } from "@/lib/ansi";
import { followPause } from "@/lib/format";

// The viewer reads one instance, named by the page it sits on, rather than
// offering a choice: a workload's output is read an instance at a time and
// the page a reader is already on says which one they meant. The page also
// says what state it last saw the instance in, which is how a follow tells
// an instance that ended from one that has not started.
const props = defineProps<{
  workload: string;
  instance: number;
  state?: InstanceState;
}>();

const tail = ref(100);
// How far back to read, in minutes. Zero reads everything the tail allows.
// The server applies this to container workloads and ignores it for exec
// ones, whose output carries no timestamps to filter on.
const since = ref(0);
const previous = ref(false);
const follow = ref(false);
// The output as lines of styled runs rather than as text, so the colours a
// program writes are drawn rather than printed as escape sequences.
const lines = ref<Span[][]>([]);
// Whether the text on screen belongs to a superseded request. It stays
// visible until the replacement's first bytes arrive, so changing a control
// repaints the content in place rather than blanking the box.
const stale = ref(false);
const error = ref("");
const loading = ref(false);
const output = ref<HTMLElement>();

// A follow reads a live stream until it ends, so there is nothing to follow
// on the attempt before the one running now. The server refuses the
// combination, and the control is disabled for the same reason rather than
// surfacing that error.
const followDisabled = computed(() => previous.value);

// Nothing has been read yet, as distinct from a read that returned nothing:
// a stream that has only opened still shows the box empty.
const empty = computed(() => !lines.value.some((line) => line.length));

let controller: AbortController | undefined;
// Whether the follow loop unticked follow itself, which the watch below
// reads so as not to reload on a change it did not make.
let ended = false;

function params(): string {
  const query = new URLSearchParams();
  query.set("tail", String(tail.value));
  if (since.value > 0) {
    query.set(
      "since",
      new Date(Date.now() - since.value * 60_000).toISOString(),
    );
  }
  query.set("instance", String(props.instance));
  if (previous.value) query.set("previous", "true");
  if (follow.value) query.set("follow", "true");
  return query.toString();
}

// append adds what a read produced to the last line, starting a new one at
// every newline. A carriage return with no newline after it rewrites the line
// instead, which is how a program draws a progress bar in place.
function append(spans: Span[]) {
  if (!lines.value.length) lines.value.push([]);

  for (const span of spans) {
    let rest = span.text;

    for (;;) {
      const at = rest.search(/[\n\r]/);
      if (at < 0) {
        if (rest)
          lines.value[lines.value.length - 1].push({ ...span, text: rest });
        break;
      }

      if (at > 0) {
        lines.value[lines.value.length - 1].push({
          ...span,
          text: rest.slice(0, at),
        });
      }

      if (rest[at] === "\n") lines.value.push([]);
      else lines.value[lines.value.length - 1] = [];

      rest = rest.slice(at + 1);
    }
  }
}

// note writes a line of the viewer's own, which is not output and is drawn as
// an aside rather than as something the workload said.
function note(text: string) {
  lines.value.push([{ text, classes: "text-slate-500 italic" }], []);
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
  // The parser is per read, since a fresh stream starts with nothing open and
  // nothing held back.
  const parse = parser();

  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;
    if (stale.value) {
      lines.value = [];
      stale.value = false;
    }
    append(parse(decoder.decode(value, { stream: true })));
    await scrollToEnd();
  }
}

// load reads the logs, and while following keeps reading. A followed stream
// ends when the server has nothing more to send: the instance ended and a
// restart is replacing it, or it has not started and there is nothing to
// send yet. The loop marks the break, worded by what the page last saw of
// the instance, waits, and asks again rather than going quiet. The state the
// page holds is a poll behind the stream, so the first break after an
// instance completes can still read as an ending; the next one, a poll
// later, is worded right and ends the follow.
async function load() {
  controller?.abort();
  const mine = new AbortController();
  controller = mine;

  stale.value = true;
  error.value = "";

  for (;;) {
    loading.value = true;
    let refusal: string | undefined;
    try {
      await read(mine.signal);

      // A response that carried nothing still replaces what it superseded.
      if (stale.value) {
        lines.value = [];
        stale.value = false;
      }
    } catch (cause) {
      if (mine.signal.aborted) return;

      // While following, a refusal is transient: the replacement instance may
      // not exist yet. The loop retries instead of reporting it, and the
      // break says what was refused rather than passing it off as an ending.
      refusal = cause instanceof Error ? cause.message : String(cause);
      if (!follow.value) error.value = refusal;
    } finally {
      loading.value = false;
    }

    if (!follow.value || mine.signal.aborted) return;

    const pause = followPause(props.state, refusal);
    note(`--- ${pause.text} ---`);
    await scrollToEnd();
    if (pause.final) {
      ended = true;
      follow.value = false;
      return;
    }

    await sleep(2000, mine.signal);
    if (mine.signal.aborted) return;
  }
}

watch(followDisabled, (disabled) => {
  if (disabled) follow.value = false;
});

watch([tail, since, previous, follow, () => props.instance], () => {
  // A follow the loop itself ended is not a change to reload on: the box
  // already shows everything the instance wrote, and a reload would take
  // the note saying why the follow ended with it.
  if (ended) {
    ended = false;
    return;
  }

  void load();
});

void load();

onUnmounted(() => controller?.abort());
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
      <label
        class="flex shrink-0 items-center gap-1.5 whitespace-nowrap text-slate-600 dark:text-slate-400"
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

      <Tooltip
        text="Container workloads only. Exec output carries no timestamps to filter on."
      >
        <label
          class="flex shrink-0 items-center gap-1.5 whitespace-nowrap text-slate-600 dark:text-slate-400"
        >
          Since
          <select
            v-model.number="since"
            class="rounded-md border border-slate-300 bg-white px-2 py-1 dark:border-slate-700 dark:bg-slate-800"
          >
            <option :value="0">start</option>
            <option :value="5">5m</option>
            <option :value="15">15m</option>
            <option :value="60">1h</option>
            <option :value="1440">24h</option>
          </select>
        </label>
      </Tooltip>

      <label
        class="flex shrink-0 items-center gap-1.5 whitespace-nowrap text-slate-600 dark:text-slate-400"
      >
        <input v-model="previous" type="checkbox" class="accent-pulse-600" />
        Previous
      </label>

      <Tooltip
        :text="
          followDisabled
            ? 'A follow reads the live stream, so turn previous off.'
            : undefined
        "
      >
        <label
          class="flex shrink-0 items-center gap-1.5 whitespace-nowrap text-slate-600 dark:text-slate-400"
          :class="{ 'opacity-50': followDisabled }"
        >
          <input
            v-model="follow"
            type="checkbox"
            :disabled="followDisabled"
            class="accent-pulse-600"
          />
          Follow
        </label>
      </Tooltip>

      <button
        v-if="!follow"
        class="rounded-md border border-slate-300 px-2.5 py-1 text-slate-600 hover:bg-slate-100 sm:ml-auto dark:border-slate-700 dark:text-slate-400 dark:hover:bg-slate-800"
        @click="load"
      >
        Refresh
      </button>
      <span
        v-else
        class="flex items-center gap-1.5 text-xs text-slate-500 sm:ml-auto dark:text-slate-400"
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

    <!-- A fixed height rather than a cap, and rendered whether or not there
         is anything to show, so nothing below the box moves as the content
         changes. -->
    <pre
      ref="output"
      class="h-96 overflow-auto border-t border-slate-200 bg-slate-950 px-4 py-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-slate-200 dark:border-slate-800"
    ><template v-if="empty">{{
        loading ? "Loading…" : "No output."
      }}</template
      ><template v-for="(line, at) in lines" v-else :key="at"
        ><span
          v-for="(span, index) in line"
          :key="index"
          :class="span.classes"
          :style="span.style"
          >{{ span.text }}</span
        >{{ "\n" }}</template
      ></pre>
  </div>
</template>
