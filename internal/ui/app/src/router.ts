import { createRouter, createWebHistory } from "vue-router";

import PlaceholderView from "./views/PlaceholderView.vue";

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
      component: PlaceholderView,
      props: { title: "Workload" },
    },
    {
      path: "/secrets",
      name: "secrets",
      component: PlaceholderView,
      props: { title: "Secrets" },
    },
    {
      path: "/variables",
      name: "variables",
      component: PlaceholderView,
      props: { title: "Variables" },
    },
    {
      path: "/volumes",
      name: "volumes",
      component: PlaceholderView,
      props: { title: "Volumes" },
    },
  ],
});
