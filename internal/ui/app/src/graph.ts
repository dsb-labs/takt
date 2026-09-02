import dagre from "@dagrejs/dagre";

import type {
  Secret,
  Variable,
  Volume,
  Workload,
  WorkloadState,
} from "./api/types";
import { references } from "./references";

// The GraphNode type is a node in the reference graph: a workload or one of
// the resources a workload can reference.
export type GraphNode = {
  id: string;
  kind: "workload" | "secret" | "variable" | "volume";
  name: string;
  state?: WorkloadState;
};

// The GraphEdge type is a dependency: the source workload references the
// target, and via says how.
export type GraphEdge = {
  id: string;
  source: string;
  target: string;
  via: string;
};

// The size every node is laid out at, shared with the node component so the
// layout and the rendering agree.
export const nodeWidth = 180;
export const nodeHeight = 48;

// buildGraph assembles the reference graph from the four lists. Edges come
// from each workload's specification, the same derivation the references card
// uses. With connectedOnly set, resources nothing references are left out,
// which is what a filtered graph wants: the neighbourhood of the workloads
// shown rather than every stray resource.
export function buildGraph(
  workloads: Workload[],
  secrets: Secret[],
  variables: Variable[],
  volumes: Volume[],
  connectedOnly: boolean,
): { nodes: GraphNode[]; edges: GraphEdge[] } {
  const nodes = new Map<string, GraphNode>();
  const edges = new Map<string, GraphEdge>();

  for (const workload of workloads) {
    nodes.set(`workload:${workload.name}`, {
      id: `workload:${workload.name}`,
      kind: "workload",
      name: workload.name,
      state: workload.state,
    });
  }

  const resource = (kind: GraphNode["kind"], name: string): string => {
    const id = `${kind}:${name}`;
    if (!nodes.has(id)) nodes.set(id, { id, kind, name });
    return id;
  };

  if (!connectedOnly) {
    for (const secret of secrets) resource("secret", secret.name);
    for (const variable of variables) resource("variable", variable.name);
    for (const volume of volumes) resource("volume", volume.name);
  }

  for (const workload of workloads) {
    const source = `workload:${workload.name}`;
    for (const ref of references(workload.spec)) {
      const target = resource(ref.kind, ref.name);
      const id = `${source}->${target}`;

      // One arrow per pair, however many ways the pair is connected.
      const existing = edges.get(id);
      edges.set(id, {
        id,
        source,
        target,
        via: existing ? `${existing.via}, ${ref.via}` : ref.via,
      });
    }
  }

  return { nodes: [...nodes.values()], edges: [...edges.values()] };
}

// layout places the nodes in layers along the dependency direction, and
// returns the top-left position of each, which is the corner the canvas
// positions by where dagre reports centres.
export function layout(
  nodes: GraphNode[],
  edges: GraphEdge[],
): Map<string, { x: number; y: number }> {
  const graph = new dagre.graphlib.Graph();
  graph.setGraph({ rankdir: "LR", nodesep: 24, ranksep: 96 });
  graph.setDefaultEdgeLabel(() => ({}));

  for (const node of nodes) {
    graph.setNode(node.id, { width: nodeWidth, height: nodeHeight });
  }
  for (const edge of edges) {
    graph.setEdge(edge.source, edge.target);
  }

  dagre.layout(graph);

  const positions = new Map<string, { x: number; y: number }>();
  for (const node of nodes) {
    const placed = graph.node(node.id);
    positions.set(node.id, {
      x: placed.x - nodeWidth / 2,
      y: placed.y - nodeHeight / 2,
    });
  }

  return positions;
}
