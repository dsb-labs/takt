import { createRouter, createWebHistory } from "vue-router";

import { identity, loadIdentity } from "./auth";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: "/login",
      name: "login",
      component: () => import("./views/login/View.vue"),
    },
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
      path: "/services",
      name: "services",
      component: () => import("./views/services/List.vue"),
    },
    {
      path: "/services/:name",
      name: "service",
      component: () => import("./views/services/Detail.vue"),
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
    {
      path: "/acl",
      name: "acl",
      component: () => import("./views/acl/View.vue"),
    },
  ],
});

// Every page needs to know who is looking at it, so the guard resolves the
// caller before anything renders. A caller the server refuses is sent to the
// login page with the page it wanted, and comes back to it after signing in.
// A server with authentication disabled answers as the anonymous admin, so
// the guard never redirects there.
router.beforeEach(async (to) => {
  const id = identity.value ?? (await loadIdentity());

  if (to.name === "login") {
    return id ? "/" : true;
  }

  if (!id) {
    return { name: "login", query: { next: to.fullPath } };
  }

  return true;
});

// The tab names what the page shows, so several open tabs can be told apart.
// A detail page names its resource and a list page names its section.
const sections: Record<string, string> = {
  login: "Sign in",
  workloads: "Workloads",
  graph: "Graph",
  secrets: "Secrets",
  "secret-new": "New secret",
  variables: "Variables",
  "variable-new": "New variable",
  volumes: "Volumes",
  services: "Services",
  acl: "Access",
};

router.afterEach((to) => {
  const name =
    typeof to.params.name === "string"
      ? to.params.name
      : sections[String(to.name)];
  document.title = name ? `${name} · takt` : "takt";
});
