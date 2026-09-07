<script setup lang="ts">
import {
  MarkerType,
  VueFlow,
  useVueFlow,
  type Edge,
  type Node,
} from "@vue-flow/core";
import { nextTick, onMounted, onUnmounted, ref, watch } from "vue";

import {
  useSecrets,
  useServices,
  useVariables,
  useVolumes,
  useWorkloads,
} from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import { useQueryFilter } from "../../filter";
import { buildGraph, layout } from "../../graph";
import GraphNode from "../../components/GraphNode.vue";

import "@vue-flow/core/dist/style.css";

const { filter, queries } = useQueryFilter();
const workloads = useWorkloads(() => queries.value);
const secrets = useSecrets(() => []);
const variables = useVariables(() => []);
const volumes = useVolumes(() => []);
const services = useServices(() => []);

const hideUnconnected = ref(false);
const nodes = ref<Node[]>([]);
const edges = ref<Edge[]>([]);

const { fitView, onNodesInitialized } = useVueFlow();

// Whether a fresh layout is waiting for its nodes to be measured. A fit that
// runs before the canvas has measured newly added nodes computes its bounds
// from the ones it already knew, which is why removing nodes recentred and
// adding them back did not.
let pendingFit = false;

onNodesInitialized(() => {
  if (!pendingFit) return;
  pendingFit = false;
  void fitView({ padding: 0.1 });
});

// The layout runs only when the set of nodes or edges changes. A poll that
// changes nothing but a workload's state recolours the nodes in place, so the
// graph does not jump under the pointer every five seconds.
let structure = "";

watch(
  [
    workloads.data,
    secrets.data,
    variables.data,
    volumes.data,
    services.data,
    hideUnconnected,
  ],
  () => {
    const graph = buildGraph(
      workloads.data.value ?? [],
      secrets.data.value ?? [],
      variables.data.value ?? [],
      volumes.data.value ?? [],
      services.data.value ?? [],
      {
        resourcesConnectedOnly: queries.value.length > 0,
        hideUnconnected: hideUnconnected.value,
      },
    );

    const key = graph.nodes
      .map((node) => node.id)
      .concat(graph.edges.map((edge) => edge.id))
      .sort()
      .join("|");

    if (key === structure) {
      nodes.value = nodes.value.map((node) => {
        const fresh = graph.nodes.find((candidate) => candidate.id === node.id);
        return fresh ? { ...node, data: fresh } : node;
      });

      return;
    }

    structure = key;
    const positions = layout(graph.nodes, graph.edges);

    nodes.value = graph.nodes.map((node) => ({
      id: node.id,
      type: "resource",
      position: positions.get(node.id)!,
      data: node,
    }));
    edges.value = graph.edges.map((edge) => ({
      id: edge.id,
      source: edge.source,
      target: edge.target,
      markerEnd: { type: MarkerType.ArrowClosed, color: "#94a3b8" },
      style: { stroke: "#94a3b8" },
    }));

    // A new layout can land outside the viewport, so the view follows it.
    // Twice: once now for layouts that only removed nodes, and once more
    // when any added nodes have been measured.
    pendingFit = true;
    void nextTick(() => fitView({ padding: 0.1 }));
  },
  { immediate: true },
);

// The canvas only fits itself once, so a resized window keeps the old
// framing until the view is told to fit again.
let refit: number | undefined;

function onResize() {
  clearTimeout(refit);
  refit = window.setTimeout(() => fitView({ padding: 0.1 }), 150);
}

onMounted(() => window.addEventListener("resize", onResize));
onUnmounted(() => {
  clearTimeout(refit);
  window.removeEventListener("resize", onResize);
});

// Awaited so Suspense holds the previous view until this one has its data.
// Failures are left for the error banner this view already renders.
await Promise.all([
  workloads.suspense(),
  secrets.suspense(),
  variables.suspense(),
  volumes.suspense(),
  services.suspense(),
]).catch(() => {});
</script>

<template>
  <div>
    <!-- On a phone the title and the checkbox share the first row and the
         filter takes a full row beneath, the way the list views with a
         button lay out. On a wider screen everything sits on one row. -->
    <div class="flex flex-wrap items-center gap-3">
      <h1 class="text-xl font-semibold">Graph</h1>
      <label
        class="order-2 ml-auto flex shrink-0 items-center gap-1.5 text-sm whitespace-nowrap text-slate-600 sm:order-3 sm:ml-0 dark:text-slate-400"
      >
        <input
          v-model="hideUnconnected"
          type="checkbox"
          class="accent-pulse-600"
        />
        Hide unconnected
      </label>
      <div class="order-3 w-full sm:order-2 sm:ml-auto sm:w-auto">
        <QueryInput v-model="filter" />
      </div>
    </div>

    <ErrorBanner
      v-if="workloads.isError.value"
      :message="`Failed to list workloads: ${workloads.error.value?.message}`"
    />

    <div
      v-else
      class="mt-6 h-[calc(100dvh-19rem)] min-h-64 overflow-hidden rounded-lg border border-slate-200 bg-slate-50 sm:h-[calc(100vh-11rem)] dark:border-slate-800 dark:bg-slate-950"
    >
      <VueFlow
        :nodes="nodes"
        :edges="edges"
        :nodes-connectable="false"
        :edges-updatable="false"
        :min-zoom="0.2"
        fit-view-on-init
      >
        <template #node-resource="{ data }">
          <GraphNode :data="data" />
        </template>
      </VueFlow>
    </div>
  </div>
</template>
