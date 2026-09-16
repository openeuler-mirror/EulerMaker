import { createRouter, createWebHistory } from "vue-router";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", name: "home", component: () => import("@/views/HomeView.vue") },
    { path: "/projects", name: "projects", component: () => import("@/views/ProjectsView.vue") },
    { path: "/projects/:name", name: "project", component: () => import("@/views/ProjectView.vue") },
    { path: "/login", name: "login", component: () => import("@/views/LoginView.vue") },
    { path: "/register", name: "register", component: () => import("@/views/RegisterView.vue") },
    { path: "/:pathMatch(.*)*", redirect: "/" },
  ],
  scrollBehavior: () => ({ top: 0 }),
});
