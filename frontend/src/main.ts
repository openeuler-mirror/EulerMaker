import "@/styles.css";

import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "@/App.vue";
import { i18n } from "@/i18n";
import { useSessionStore } from "@/stores/session";
import { captureOAuthCallback } from "@/oauth";

captureOAuthCallback();
const { router } = await import("@/router");

const app = createApp(App);
const pinia = createPinia();

app.use(pinia).use(i18n);
await useSessionStore(pinia).restore();
app.use(router);
app.mount("#app");
