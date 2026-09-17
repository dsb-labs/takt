import { describe, expect, it } from "vitest";

import type { Secret, Service, Variable, Volume, Workload } from "@/api/types";
import { buildGraph, layout, nodeWidth, type GraphEdge } from "@/lib/graph";

const at = "2026-01-01T00:00:00Z";

function workload(name: string, spec: Partial<Workload["spec"]>): Workload {
  return {
    name,
    version: 1,
    runtime: "container",
    state: "running",
    spec: { version: "v1", name, ...spec },
    createdAt: at,
    updatedAt: at,
  };
}

const secret = (name: string): Secret => ({
  name,
  revision: "1",
  createdAt: at,
  updatedAt: at,
});
const variable = (name: string): Variable => ({
  name,
  value: "",
  createdAt: at,
  updatedAt: at,
});
const volume = (name: string): Volume => ({ name, createdAt: at });
const service = (name: string, labels: Record<string, string>): Service => ({
  name,
  target: { labels, port: 80 },
  createdAt: at,
  updatedAt: at,
});

const everything = { resourcesConnectedOnly: false, hideUnconnected: false };

// A fixture with one of everything: an api workload that reads a secret two
// ways and mounts a volume, a worker nothing selects, a service selecting
// the api, and a stray secret nothing references.
const workloads = [
  workload("api", {
    labels: { app: "api" },
    env: { DSN: "${secret:db}", KEY: "${secret:db}" },
    volumes: [{ name: "data", to: "/data" }],
  }),
  workload("worker", { env: { PEER: "${workload:api:8080}" } }),
];
const secrets = [secret("db"), secret("unused")];
const volumes = [volume("data")];
const services = [service("web", { app: "api" })];

function edge(edges: GraphEdge[], source: string, target: string) {
  return edges.find((e) => e.source === source && e.target === target);
}

describe("buildGraph", () => {
  it("draws every node and edge by default", () => {
    const { nodes, edges } = buildGraph(
      workloads,
      secrets,
      [variable("v")],
      volumes,
      services,
      everything,
    );

    expect(nodes.map((node) => node.id).sort()).toEqual([
      "secret:db",
      "secret:unused",
      "service:web",
      "variable:v",
      "volume:data",
      "workload:api",
      "workload:worker",
    ]);
    expect(edges).toHaveLength(4);
  });

  it("carries a workload's state onto its node", () => {
    const { nodes } = buildGraph(workloads, [], [], [], [], everything);

    expect(nodes.find((node) => node.id === "workload:api")?.state).toBe(
      "running",
    );
  });

  it("draws one arrow per pair, naming every way they are joined", () => {
    const { edges } = buildGraph(workloads, secrets, [], [], [], everything);

    expect(edge(edges, "workload:api", "secret:db")?.via).toBe(
      "env DSN, env KEY",
    );
  });

  it("draws a service as selecting the workloads carrying its labels", () => {
    const { edges } = buildGraph(workloads, [], [], [], services, everything);

    expect(edge(edges, "service:web", "workload:api")?.via).toBe("selects");
    expect(edge(edges, "service:web", "workload:worker")).toBeUndefined();
  });

  it("adds a resource a workload names even when the list lacks it", () => {
    const { nodes } = buildGraph(workloads, [], [], [], [], everything);

    expect(nodes.map((node) => node.id)).toContain("secret:db");
  });

  it("leaves a token out, since it names no resource", () => {
    const { nodes, edges } = buildGraph(
      [workload("api", { env: { T: "${token:ci}" } })],
      [],
      [],
      [],
      [],
      everything,
    );

    expect(nodes).toHaveLength(1);
    expect(edges).toHaveLength(0);
  });

  it("leaves out the resources nothing references when asked", () => {
    const { nodes } = buildGraph(workloads, secrets, [], volumes, services, {
      resourcesConnectedOnly: true,
      hideUnconnected: false,
    });

    const ids = nodes.map((node) => node.id);
    expect(ids).not.toContain("secret:unused");
    expect(ids).toContain("secret:db");
    // A service still joins through what it selects.
    expect(ids).toContain("service:web");
  });

  it("leaves out every node without an edge when asked", () => {
    const { nodes } = buildGraph(
      [...workloads, workload("lonely", {})],
      secrets,
      [],
      volumes,
      [],
      { resourcesConnectedOnly: false, hideUnconnected: true },
    );

    const ids = nodes.map((node) => node.id);
    expect(ids).not.toContain("workload:lonely");
    expect(ids).not.toContain("secret:unused");
    // The worker references the api, so both stay.
    expect(ids).toContain("workload:worker");
    expect(ids).toContain("workload:api");
  });
});

describe("layout", () => {
  it("places a source to the left of what it points at", () => {
    const { nodes, edges } = buildGraph(workloads, [], [], [], [], everything);
    const positions = layout(nodes, edges);

    const worker = positions.get("workload:worker")!;
    const api = positions.get("workload:api")!;
    const db = positions.get("secret:db")!;

    expect(worker.x + nodeWidth).toBeLessThanOrEqual(api.x);
    expect(api.x + nodeWidth).toBeLessThanOrEqual(db.x);
  });

  it("reports the top-left corner of each node", () => {
    const positions = layout(
      [{ id: "only", kind: "workload", name: "only" }],
      [],
    );

    // Dagre centres a lone node half a node in from the origin, so the corner
    // the layout reports is the origin itself.
    expect(positions.get("only")).toEqual({ x: 0, y: 0 });
  });
});
