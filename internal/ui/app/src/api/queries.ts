import { useQuery } from "@tanstack/vue-query";

import { client } from "./client";

// How often the list views ask the server again. Polling matches the server's
// level-triggered reconciliation model: there is no change feed to subscribe
// to, and the reconciler itself only looks this often.
export const pollInterval = 5000;

export function useWorkloads() {
  return useQuery({
    queryKey: ["workloads"],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/workloads");
      if (error) throw new Error(error.error);
      return data.workloads;
    },
  });
}

export function useWorkload(name: () => string) {
  return useQuery({
    queryKey: ["workloads", name],
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

export function useSecrets() {
  return useQuery({
    queryKey: ["secrets"],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/secrets");
      if (error) throw new Error(error.error);
      return data.secrets;
    },
  });
}

export function useVariables() {
  return useQuery({
    queryKey: ["variables"],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/variables");
      if (error) throw new Error(error.error);
      return data.variables;
    },
  });
}

export function useVolumes() {
  return useQuery({
    queryKey: ["volumes"],
    refetchInterval: pollInterval,
    queryFn: async () => {
      const { data, error } = await client.GET("/api/v1/volumes");
      if (error) throw new Error(error.error);
      return data.volumes;
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
      const { data, error } = await client.GET("/api/v1/ready");
      if (data) return data;
      if (error && "ready" in error) return error;
      throw new Error("failed to read readiness");
    },
  });
}
