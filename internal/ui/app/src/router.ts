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
      path: "/variables",
      name: "variables",
      component: () => import("./views/VariablesView.vue"),
    },
    {
      path: "/volumes",
      name: "volumes",
      component: () => import("./views/VolumesView.vue"),
    },
  ],
});
