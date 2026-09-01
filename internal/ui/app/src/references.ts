import type { WorkloadSpec } from "./api/types";

// A Reference is something a workload's specification names: a secret,
// variable or another workload expanded into its environment, or a volume,
// secret or variable mounted as a file.
export type Reference = {
  kind: "secret" | "variable" | "volume" | "workload";
  name: string;
  via: string;
};

// The env expansion syntax: ${secret:name}, ${var:name} and
// ${workload:name:port}, where anything after the name is ignored here.
const pattern = /\$\{(secret|var|workload):([^}:]+)[^}]*\}/g;

const kinds = {
  secret: "secret",
  var: "variable",
  workload: "workload",
} as const;

// references lists everything the given specification refers to, in the order
// the specification declares it: environment expansions first, mounts second.
export function references(spec: WorkloadSpec): Reference[] {
  const refs: Reference[] = [];

  for (const [key, value] of Object.entries(spec.env ?? {})) {
    for (const match of value.matchAll(pattern)) {
      refs.push({
        kind: kinds[match[1] as keyof typeof kinds],
        name: match[2]!,
        via: `env ${key}`,
      });
    }
  }

  for (const mount of spec.volumes ?? []) {
    const via = `mounted at ${mount.to}`;
    if (mount.name) refs.push({ kind: "volume", name: mount.name, via });
    if (mount.secret) refs.push({ kind: "secret", name: mount.secret, via });
    if (mount.var) refs.push({ kind: "variable", name: mount.var, via });
  }

  return refs;
}

// referenceTarget returns the route a reference links to. Secrets, variables
// and volumes link to their list pages, another workload to its detail page.
export function referenceTarget(ref: Reference): string {
  switch (ref.kind) {
    case "workload":
      return `/workloads/${ref.name}`;
    case "secret":
      return "/secrets";
    case "variable":
      return "/variables";
    case "volume":
      return "/volumes";
  }
}
