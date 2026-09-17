import { describe, expect, it } from "vitest";

import type { WorkloadSpec } from "@/api/types";
import {
  hostPaths,
  mountedAt,
  references,
  referenceTarget,
  type Reference,
} from "@/lib/references";

function spec(partial: Partial<WorkloadSpec>): WorkloadSpec {
  return { version: "v1", name: "example", ...partial };
}

describe("references", () => {
  it("finds nothing in a specification naming nothing", () => {
    expect(references(spec({}))).toEqual([]);
  });

  it("reads every expansion an environment value carries", () => {
    const refs = references(
      spec({
        env: {
          DSN: "postgres://${secret:db-user}:${secret:db-pass}@${workload:db:5432}/app",
          MODE: "${var:mode}",
          TOKEN: "${token:ci}",
          PLAIN: "no expansion here",
        },
      }),
    );

    expect(refs).toEqual([
      { kind: "secret", name: "db-user", via: "env DSN" },
      { kind: "secret", name: "db-pass", via: "env DSN" },
      { kind: "workload", name: "db", via: "env DSN" },
      { kind: "variable", name: "mode", via: "env MODE" },
      { kind: "token", name: "ci", via: "env TOKEN" },
    ]);
  });

  it("reads every mount, and leaves a host path out", () => {
    const refs = references(
      spec({
        volumes: [
          { name: "data", to: "/data" },
          { secret: "cert", to: "/etc/cert", readOnly: true },
          { var: "config", to: "/etc/config" },
          { token: "ci", to: "/run/token" },
          { path: "/host", to: "/mnt/host" },
        ],
      }),
    );

    expect(refs).toEqual([
      { kind: "volume", name: "data", via: "mounted at /data" },
      { kind: "secret", name: "cert", via: "mounted read-only at /etc/cert" },
      { kind: "variable", name: "config", via: "mounted at /etc/config" },
      { kind: "token", name: "ci", via: "mounted at /run/token" },
    ]);
  });

  it("lists expansions before mounts", () => {
    const refs = references(
      spec({
        env: { A: "${var:first}" },
        volumes: [{ name: "second", to: "/second" }],
      }),
    );

    expect(refs.map((ref) => ref.name)).toEqual(["first", "second"]);
  });
});

describe("mountedAt", () => {
  it.each([
    [{ to: "/data" }, "mounted at /data"],
    [{ to: "/data", readOnly: true }, "mounted read-only at /data"],
    [{ to: "/data", propagation: "rslave" }, "mounted at /data, rslave"],
    [
      { to: "/data", readOnly: true, propagation: "rshared" },
      "mounted read-only at /data, rshared",
    ],
  ] as const)("describes %o as %s", (mount, expected) => {
    expect(mountedAt(mount)).toBe(expected);
  });
});

describe("hostPaths", () => {
  it("lists only the host paths", () => {
    const paths = hostPaths(
      spec({
        volumes: [
          { name: "data", to: "/data" },
          { path: "/var/run/docker.sock", to: "/var/run/docker.sock" },
        ],
      }),
    );

    expect(paths).toEqual([
      {
        kind: "path",
        name: "/var/run/docker.sock",
        via: "mounted at /var/run/docker.sock",
      },
    ]);
  });
});

describe("referenceTarget", () => {
  it.each<[Reference["kind"], string]>([
    ["workload", "/workloads/x"],
    ["secret", "/secrets/x"],
    ["variable", "/variables/x"],
    ["volume", "/volumes/x"],
    ["service", "/services/x"],
    ["token", "/acl"],
    ["path", ""],
  ])("routes a %s to %s", (kind, expected) => {
    expect(referenceTarget({ kind, name: "x", via: "" })).toBe(expected);
  });
});
