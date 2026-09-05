import type { VolumeMount, WorkloadSpec } from "./api/types";

// A Reference is something a workload's specification names: a secret,
// variable or another workload expanded into its environment, or a volume,
// secret or variable mounted as a file.
export type Reference = {
  kind: "secret" | "variable" | "volume" | "workload" | "service";
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
    const via = mountedAt(mount);
    if (mount.name) refs.push({ kind: "volume", name: mount.name, via });
    if (mount.secret) refs.push({ kind: "secret", name: mount.secret, via });
    if (mount.var) refs.push({ kind: "variable", name: mount.var, via });
  }

  return refs;
}

// mountedAt describes where a workload finds a mount, carrying the read-only
// flag so a restricted mount reads differently from a writable one.
export function mountedAt(mount: VolumeMount): string {
  return mount.readOnly
    ? `mounted read-only at ${mount.to}`
    : `mounted at ${mount.to}`;
}

// hostPaths lists the host paths the given specification mounts. They are not
// references: a path mount reaches outside orca-managed state, so there is no
// resource to link to.
export function hostPaths(spec: WorkloadSpec): { path: string; via: string }[] {
  const paths: { path: string; via: string }[] = [];

  for (const mount of spec.volumes ?? []) {
    if (mount.path) paths.push({ path: mount.path, via: mountedAt(mount) });
  }

  return paths;
}

// referenceTarget returns the detail route a reference links to.
export function referenceTarget(ref: Reference): string {
  switch (ref.kind) {
    case "workload":
      return `/workloads/${ref.name}`;
    case "secret":
      return `/secrets/${ref.name}`;
    case "variable":
      return `/variables/${ref.name}`;
    case "volume":
      return `/volumes/${ref.name}`;
    case "service":
      return `/services/${ref.name}`;
  }
}
