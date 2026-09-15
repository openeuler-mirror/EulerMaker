import { createRouter, createWebHistory } from "vue-router";

const StartView = { template: "<section><h1>EulerMaker</h1></section>" };

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", name: "home", component: StartView },
    { path: "/:pathMatch(.*)*", redirect: "/" },
  ],
});
