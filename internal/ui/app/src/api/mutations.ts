import { useMutation, useQueryClient } from "@tanstack/vue-query";

import { client } from "./client";

// Every mutation invalidates the queries reading what it changed, so the view
// reflects the action on the next render rather than the next poll.

export function useWorkloadAction(
  action: "stop" | "start" | "restart",
  name: () => string,
) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async () => {
      const { error } = await client.POST(
        `/api/v1/workloads/{name}/${action}`,
        {
          params: { path: { name: name() } },
          body: {},
        },
      );
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["workloads"] }),
  });
}

export function useDeleteWorkload(name: () => string) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async () => {
      const { error } = await client.DELETE("/api/v1/workloads/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["workloads"] }),
  });
}

export function useSetSecret() {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async ({ name, value }: { name: string; value: string }) => {
      const { error } = await client.PUT("/api/v1/secrets/{name}", {
        params: { path: { name } },
        body: { value },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["secrets"] }),
  });
}

export function useDeleteSecret() {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (name: string) => {
      const { error } = await client.DELETE("/api/v1/secrets/{name}", {
        params: { path: { name } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["secrets"] }),
  });
}

export function useSetVariable() {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async ({ name, value }: { name: string; value: string }) => {
      const { error } = await client.PUT("/api/v1/variables/{name}", {
        params: { path: { name } },
        body: { value },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["variables"] }),
  });
}

export function useDeleteVariable() {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (name: string) => {
      const { error } = await client.DELETE("/api/v1/variables/{name}", {
        params: { path: { name } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["variables"] }),
  });
}

export function useDeleteVolume() {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (name: string) => {
      const { error } = await client.DELETE("/api/v1/volumes/{name}", {
        params: { path: { name } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["volumes"] }),
  });
}
