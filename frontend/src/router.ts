import { createRouter, createWebHistory } from "vue-router";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", name: "home", component: () => import("@/views/HomeView.vue") },
    { path: "/projects", name: "projects", component: () => import("@/views/ProjectsView.vue") },
    { path: "/login", name: "login", component: () => import("@/views/LoginView.vue") },
    { path: "/:pathMatch(.*)*", redirect: "/" },
  ],
  scrollBehavior: () => ({ top: 0 }),
});
