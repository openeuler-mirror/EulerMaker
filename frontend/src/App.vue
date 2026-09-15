<template>
  <div class="app-shell">
    <header class="topbar">
      <RouterLink class="brand" to="/" :aria-label="`${t('app.title')} ${t('app.home')}`">
        <img src="@/assets/eulermaker-logo.svg" alt="" />
        <span>EulerMaker</span>
      </RouterLink>

      <button class="menu-toggle" type="button" :aria-label="t('app.openNavigation')" @click="menuOpen = !menuOpen">
        <Menu />
      </button>

      <nav :class="['main-nav', { open: menuOpen }]" :aria-label="t('app.mainNavigation')" @click="menuOpen = false">
        <RouterLink to="/">{{ t("app.home") }}</RouterLink>
        <RouterLink to="/projects">{{ t("app.projects") }}</RouterLink>
      </nav>

      <div class="account">
        <button class="language-button" type="button" :aria-label="t('app.switchLanguage')" @click="toggleLocale">
          {{ locale === "zh-CN" ? "EN" : "中" }}
        </button>
        <template v-if="session.authenticated">
          <span class="identity"><User />{{ session.username }}</span>
          <button type="button" class="text-button" @click="logout">{{ t("app.logout") }}</button>
        </template>
        <RouterLink v-else class="login-button" to="/login">{{ t("app.login") }}</RouterLink>
      </div>
    </header>

    <main class="page-container">
      <RouterView />
    </main>

    <footer class="footer">
      <div class="footer-copy"><strong>EulerMaker</strong><span>{{ t("app.footer") }}</span></div>
      <nav class="footer-links" :aria-label="t('app.footerLinks')">
        <a :href="openEulerLink('privacy')" target="_blank" rel="noopener noreferrer">{{ t("app.privacy") }}</a>
        <a :href="openEulerLink('legal')" target="_blank" rel="noopener noreferrer">{{ t("app.legal") }}</a>
        <a :href="openEulerLink('cookies')" target="_blank" rel="noopener noreferrer">{{ t("app.cookies") }}</a>
      </nav>
    </footer>
  </div>
</template>

<script setup lang="ts">
import { Menu, User } from "@element-plus/icons-vue";
import { ref, watch } from "vue";
import { RouterLink, RouterView, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";

import { LOCALE_KEY, type Locale } from "@/i18n";
import { useSessionStore } from "@/stores/session";

const menuOpen = ref(false);
const session = useSessionStore();
const router = useRouter();
const { t, locale } = useI18n();

watch(
  locale,
  (value) => {
    document.documentElement.lang = value;
    document.title = t("app.title");
    document.querySelector('meta[name="description"]')?.setAttribute("content", t("app.description"));
  },
  { immediate: true },
);

function toggleLocale(): void {
  const next: Locale = locale.value === "zh-CN" ? "en-US" : "zh-CN";
  locale.value = next;
  localStorage.setItem(LOCALE_KEY, next);
}

function openEulerLink(kind: "privacy" | "legal" | "cookies"): string {
  const prefix = locale.value === "en-US" ? "en" : "zh";
  return `https://www.openeuler.org/${prefix}/other/${kind}/`;
}

async function logout(): Promise<void> {
  session.signOut();
  await router.push("/");
}
</script>
