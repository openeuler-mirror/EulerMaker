import { createRouter, createWebHistory } from "vue-router";
import { useSessionStore } from "@/stores/session";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", name: "home", component: () => import("@/views/HomeView.vue") },
    { path: "/projects", name: "projects", component: () => import("@/views/ProjectsView.vue") },
    { path: "/projects/:name", name: "project", component: () => import("@/views/ProjectView.vue") },
    { path: "/login", name: "login", component: () => import("@/views/LoginView.vue") },
    { path: "/register", name: "register", component: () => import("@/views/RegisterView.vue") },
    { path: "/admin/users", name: "admin-users", component: () => import("@/views/AdminUsersView.vue"), meta: { roles: ["admin"] } },
    { path: "/admin/machineaccounts", name: "admin-machineaccounts", component: () => import("@/views/MachineAccountsView.vue"), meta: { roles: ["admin"] } },
    { path: "/operations", alias: "/runners", name: "operations", component: () => import("@/views/OperationsView.vue"), meta: { roles: ["ops", "admin"] } },
    { path: "/settings", name: "settings", component: () => import("@/views/SettingsView.vue"), meta: { authenticated: true } },
    { path: "/:pathMatch(.*)*", redirect: "/" },
  ],
  scrollBehavior: () => ({ top: 0 }),
});

router.beforeEach((to) => {
  const roles = to.meta.roles as string[] | undefined;
  if (!roles && !to.meta.authenticated) return true;
  const session = useSessionStore();
  if (!session.authenticated) return { name: "login", query: { redirect: to.fullPath } };
  return !roles || roles.includes(session.role) ? true : { name: "home" };
});
