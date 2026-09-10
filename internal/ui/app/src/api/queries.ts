import { useQuery } from "@tanstack/vue-query";
import { computed } from "vue";

import { client } from "./client";

// How often the list views ask the server again. Polling matches the server's
// level-triggered reconciliation model: there is no change feed to subscribe
// to, and the reconciler itself only looks this often.
export const pollInterval = 5000;

export function useWorkloads(query: () => string[]) {
  return useQuery({
    queryKey: ["workloads", computed(query)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/workloads", {
        params: { query: { query: query() } },
      });
      if (error) throw new Error(error.error);
      return data.workloads;
    },
  });
}

export function useWorkload(name: () => string) {
  return useQuery({
    queryKey: ["workloads", computed(name)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/workloads/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
      return data.workload;
    },
  });
}

export function useSecrets(query: () => string[]) {
  return useQuery({
    queryKey: ["secrets", computed(query)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/secrets", {
        params: { query: { query: query() } },
      });
      if (error) throw new Error(error.error);
      return data.secrets;
    },
  });
}

export function useVariables(query: () => string[]) {
  return useQuery({
    queryKey: ["variables", computed(query)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/variables", {
        params: { query: { query: query() } },
      });
      if (error) throw new Error(error.error);
      return data.variables;
    },
  });
}

export function useVolumes(query: () => string[]) {
  return useQuery({
    queryKey: ["volumes", computed(query)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/volumes", {
        params: { query: { query: query() } },
      });
      if (error) throw new Error(error.error);
      return data.volumes;
    },
  });
}

export function useServices(query: () => string[]) {
  return useQuery({
    queryKey: ["services", computed(query)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/services", {
        params: { query: { query: query() } },
      });
      if (error) throw new Error(error.error);
      return data.services;
    },
  });
}

export function useService(name: () => string) {
  return useQuery({
    queryKey: ["services", computed(name)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/services/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
      return data.service;
    },
  });
}

export function useSecret(name: () => string) {
  return useQuery({
    queryKey: ["secrets", computed(name)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/secrets/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
      return data.secret;
    },
  });
}

export function useVariable(name: () => string) {
  return useQuery({
    queryKey: ["variables", computed(name)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/variables/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
      return data.variable;
    },
  });
}

export function useVolume(name: () => string) {
  return useQuery({
    queryKey: ["volumes", computed(name)],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/volumes/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
      return data.volume;
    },
  });
}

export function useReadiness() {
  return useQuery({
    queryKey: ["readiness"],
    refetchInterval: pollInterval,
    retry: false,
    queryFn: async () => {
      // A not-ready server answers with a 503 carrying the same shape, so the
      // response is read either way rather than treated as a failure.
      const { data, error } = await client.GET("/api/v1/system/ready");
      if (data) return data;
      if (error && "ready" in error) return error;
      throw new Error("failed to read readiness");
    },
  });
}

// Whether the server offers the browser OIDC flow, discovered by asking for
// it: the route answers 404 when the configuration names no issuer, which is
// how "sign in with SSO" knows whether to exist. Asked once, not polled.
export function useOIDC() {
  return useQuery({
    queryKey: ["oidc"],
    retry: false,
    queryFn: async () => {
      const { data } = await client.GET("/api/v1/auth/oidc");
      return data ?? null;
    },
  });
}
