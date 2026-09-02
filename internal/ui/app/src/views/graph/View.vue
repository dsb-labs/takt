<script setup lang="ts">
import { MarkerType, VueFlow, type Edge, type Node } from "@vue-flow/core";
import { ref, watch } from "vue";

import {
  useSecrets,
  useVariables,
  useVolumes,
  useWorkloads,
} from "../../api/queries";
import ErrorBanner from "../../components/ErrorBanner.vue";
import QueryInput from "../../components/QueryInput.vue";
import { useQueryFilter } from "../../filter";
import { buildGraph, layout } from "../../graph";
import GraphNode from "./Node.vue";

import "@vue-flow/core/dist/style.css";

const { filter, queries } = useQueryFilter();
const workloads = useWorkloads(() => queries.value);
const secrets = useSecrets(() => []);
const variables = useVariables(() => []);
const volumes = useVolumes(() => []);

const nodes = ref<Node[]>([]);
const edges = ref<Edge[]>([]);

// The layout runs only when the set of nodes or edges changes. A poll that
// changes nothing but a workload's state recolours the nodes in place, so the
// graph does not jump under the pointer every five seconds.
let structure = "";

watch(
  [workloads.data, secrets.data, variables.data, volumes.data],
  () => {
    const graph = buildGraph(
      workloads.data.value ?? [],
      secrets.data.value ?? [],
      variables.data.value ?? [],
      volumes.data.value ?? [],
      queries.value.length > 0,
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
  },
  { immediate: true },
);

// Awaited so Suspense holds the previous view until this one has its data.
// Failures are left for the error banner this view already renders.
await Promise.all([
  workloads.suspense(),
  secrets.suspense(),
  variables.suspense(),
  volumes.suspense(),
]).catch(() => {});
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-xl font-semibold">Graph</h1>
      <QueryInput v-model="filter" />
    </div>

    <ErrorBanner
      v-if="workloads.isError.value"
      :message="`Failed to list workloads: ${workloads.error.value?.message}`"
    />

    <div
      v-else
      class="mt-6 h-[calc(100vh-11rem)] min-h-96 overflow-hidden rounded-lg border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
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
