import { createRouter, createWebHistory } from "vue-router";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: "/",
      name: "workloads",
      component: () => import("./views/workloads/List.vue"),
    },
    {
      path: "/workloads/:name",
      name: "workload",
      component: () => import("./views/workloads/Detail.vue"),
    },
    {
      path: "/graph",
      name: "graph",
      component: () => import("./views/graph/View.vue"),
    },
    {
      path: "/secrets",
      name: "secrets",
      component: () => import("./views/secrets/List.vue"),
    },
    {
      path: "/secrets/new",
      name: "secret-new",
      component: () => import("./views/secrets/New.vue"),
    },
    {
      path: "/secrets/:name",
      name: "secret",
      component: () => import("./views/secrets/Detail.vue"),
    },
    {
      path: "/variables",
      name: "variables",
      component: () => import("./views/variables/List.vue"),
    },
    {
      path: "/variables/new",
      name: "variable-new",
      component: () => import("./views/variables/New.vue"),
    },
    {
      path: "/variables/:name",
      name: "variable",
      component: () => import("./views/variables/Detail.vue"),
    },
    {
      path: "/volumes",
      name: "volumes",
      component: () => import("./views/volumes/List.vue"),
    },
    {
      path: "/volumes/:name",
      name: "volume",
      component: () => import("./views/volumes/Detail.vue"),
    },
  ],
});
