<script setup lang="ts">
import { Handle, Position } from "@vue-flow/core";
import { useRouter } from "vue-router";

import type { GraphNode } from "../graph";
import { referenceTarget } from "../references";

const props = defineProps<{ data: GraphNode }>();
const router = useRouter();

const accents: Record<GraphNode["kind"], string> = {
  workload: "border-l-ocean-500",
  secret: "border-l-amber-500",
  variable: "border-l-emerald-500",
  volume: "border-l-violet-500",
  service: "border-l-rose-500",
};

const stateDots: Record<string, string> = {
  running: "bg-emerald-500",
  degraded: "bg-amber-500",
  pending: "bg-sky-500",
  failed: "bg-rose-500",
  stopped: "bg-slate-400",
  completed: "bg-ocean-500",
  suspended: "bg-violet-500",
  terminating: "bg-slate-400",
};

function open() {
  void router.push(
    referenceTarget({ kind: props.data.kind, name: props.data.name, via: "" }),
  );
}
</script>

<template>
  <div
    class="h-12 w-45 cursor-pointer rounded-md border border-l-4 border-slate-200 bg-white px-3 py-1.5 hover:border-slate-300 dark:border-slate-700 dark:bg-slate-800 dark:hover:border-slate-600"
    :class="accents[data.kind]"
    :title="data.name"
    @click="open"
  >
    <div
      class="text-[10px] tracking-wide text-slate-400 uppercase dark:text-slate-500"
    >
      {{ data.kind }}
    </div>
    <div
      class="flex items-center gap-1.5 truncate font-mono text-xs text-slate-900 dark:text-slate-100"
    >
      <span
        v-if="data.state"
        class="h-2 w-2 shrink-0 rounded-full"
        :class="stateDots[data.state]"
        :title="data.state"
      ></span>
      <span class="truncate">{{ data.name }}</span>
    </div>
    <Handle
      type="target"
      :position="Position.Left"
      class="!border-0 !bg-transparent"
    />
    <Handle
      type="source"
      :position="Position.Right"
      class="!border-0 !bg-transparent"
    />
  </div>
</template>
