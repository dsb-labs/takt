import { createRouter, createWebHistory } from "vue-router";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: "/",
      name: "dashboard",
      component: () => import("./views/DashboardView.vue"),
    },
    {
      path: "/workloads/:name",
      name: "workload",
      component: () => import("./views/WorkloadView.vue"),
    },
    {
      path: "/secrets",
      name: "secrets",
      component: () => import("./views/SecretsView.vue"),
    },
    {
      path: "/secrets/new",
      name: "secret-new",
      component: () => import("./views/SecretNewView.vue"),
    },
    {
      path: "/secrets/:name",
      name: "secret",
      component: () => import("./views/SecretView.vue"),
    },
    {
      path: "/variables",
      name: "variables",
      component: () => import("./views/VariablesView.vue"),
    },
    {
      path: "/variables/new",
      name: "variable-new",
      component: () => import("./views/VariableNewView.vue"),
    },
    {
      path: "/variables/:name",
      name: "variable",
      component: () => import("./views/VariableView.vue"),
    },
    {
      path: "/volumes",
      name: "volumes",
      component: () => import("./views/VolumesView.vue"),
    },
    {
      path: "/volumes/:name",
      name: "volume",
      component: () => import("./views/VolumeView.vue"),
    },
  ],
});
