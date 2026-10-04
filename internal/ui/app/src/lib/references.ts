import type { Variable, VolumeMount, WorkloadSpec } from "@/api/types";

// A Reference is something a workload's specification names: a secret,
// variable, token or another workload expanded into its environment, or a
// volume, secret, variable or token mounted as a file. A host path names no
// takt resource, so it renders without a link and joins no graph; a token
// names a principal rather than a resource, so it links to the credentials
// view and joins no graph either.
export type Reference = {
  kind:
    | "secret"
    | "variable"
    | "volume"
    | "workload"
    | "service"
    | "path"
    | "token";
  name: string;
  via: string;
};

// The env expansion syntax: ${secret:name}, ${var:name}, ${token:principal}
// and ${workload:name:port}, where anything after the name is ignored here.
const pattern = /\$\{(secret|var|workload|token):([^}:]+)[^}]*\}/g;

const kinds = {
  secret: "secret",
  var: "variable",
  workload: "workload",
  token: "token",
} as const;

// references lists everything the given specification refers to, in the order
// the specification declares it: environment expansions first, mounts second,
// and after each mount with expand set, what the mounted variable's value
// names. That last list needs the variables, so a caller without them gets
// the mounts alone.
//
// One level only, the same as the server: a ${var:} inside the value is a
// reference to that variable, not to what it names. A ${token:} inside is
// left out, because the server refuses one there and nothing can be read
// through it.
export function references(
  spec: WorkloadSpec,
  variables: Pick<Variable, "name" | "value">[] = [],
): Reference[] {
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

  const values = new Map(variables.map((v) => [v.name, v.value]));

  for (const mount of spec.volumes ?? []) {
    const via = mountedAt(mount);
    if (mount.name) refs.push({ kind: "volume", name: mount.name, via });
    if (mount.secret) refs.push({ kind: "secret", name: mount.secret, via });
    if (mount.var) refs.push({ kind: "variable", name: mount.var, via });
    if (mount.token) refs.push({ kind: "token", name: mount.token, via });

    if (!mount.expand || !mount.var) continue;

    // A variable nothing holds yet is only the mount's own reference above,
    // which is how the server reports it missing too.
    const value = values.get(mount.var);
    if (value === undefined) continue;

    for (const match of value.matchAll(pattern)) {
      const kind = kinds[match[1] as keyof typeof kinds];
      if (kind === "token") continue;

      refs.push({ kind, name: match[2]!, via: `variable ${mount.var}` });
    }
  }

  return refs;
}

// mountedAt describes where a workload finds a mount, carrying the read-only
// flag and the propagation so a restricted or propagating mount reads
// differently from a plain one.
export function mountedAt(mount: VolumeMount): string {
  const at = mount.readOnly
    ? `mounted read-only at ${mount.to}`
    : `mounted at ${mount.to}`;

  return mount.propagation ? `${at}, ${mount.propagation}` : at;
}

// hostPaths lists the host paths the given specification mounts, shaped as
// references so they share the reference card's table. They stay out of
// references() on purpose: the graph consumes that, and a host path is not a
// resource for it to draw.
export function hostPaths(spec: WorkloadSpec): Reference[] {
  const paths: Reference[] = [];

  for (const mount of spec.volumes ?? []) {
    if (mount.path)
      paths.push({ kind: "path", name: mount.path, via: mountedAt(mount) });
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
    case "token":
      // A token names a principal rather than a resource of its own, so the
      // credentials view is where it is seen.
      return "/acl";
    case "path":
      // A host path is not an takt resource, so there is nowhere to go.
      return "";
  }
}
