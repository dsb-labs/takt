import { useMutation, useQueryClient } from "@tanstack/vue-query";

import { client } from "./client";

// Every mutation invalidates the queries reading what it changed, so the view
// reflects the action on the next render rather than the next poll.
//
// Each delete takes a force flag. An unforced delete of something another
// workload reads is refused with the reason, and the view offers to force it.

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
    mutationFn: async (force: boolean) => {
      const { error } = await client.DELETE("/api/v1/workloads/{name}", {
        params: { path: { name: name() }, query: { force } },
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

export function useDeleteSecret(name: () => string) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (force: boolean) => {
      const { error } = await client.DELETE("/api/v1/secrets/{name}", {
        params: { path: { name: name() }, query: { force } },
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

export function useDeleteVariable(name: () => string) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (force: boolean) => {
      const { error } = await client.DELETE("/api/v1/variables/{name}", {
        params: { path: { name: name() }, query: { force } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["variables"] }),
  });
}

// A service's delete takes no force: nothing ever holds a service, so the
// argument DeleteControl passes is accepted and ignored.
export function useDeleteService(name: () => string) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (force: boolean) => {
      void force;
      const { error } = await client.DELETE("/api/v1/services/{name}", {
        params: { path: { name: name() } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["services"] }),
  });
}

export function useDeleteVolume(name: () => string) {
  const queries = useQueryClient();

  return useMutation({
    mutationFn: async (force: boolean) => {
      const { error } = await client.DELETE("/api/v1/volumes/{name}", {
        params: { path: { name: name() }, query: { force } },
      });
      if (error) throw new Error(error.error);
    },
    onSuccess: () => queries.invalidateQueries({ queryKey: ["volumes"] }),
  });
}

// useLogin exchanges a pasted token for a session rather than storing it:
// the session cookie is HttpOnly and expires on its own, so the standing
// credential never lives in the browser.
export function useLogin() {
  return useMutation({
    mutationFn: async (token: string) => {
      const { error } = await client.POST("/api/v1/auth", {
        body: { token, cookie: true },
      });
      if (error) throw new Error(error.error);
    },
  });
}

// useLogout revokes the session server-side, so signing out is a revocation
// rather than only a forgotten cookie.
export function useLogout() {
  return useMutation({
    mutationFn: async () => {
      const { error } = await client.DELETE("/api/v1/auth");
      if (error) throw new Error(error.error);
    },
  });
}
