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
        <RouterLink v-if="session.role === 'ops' || session.role === 'admin'" to="/operations">{{ t("app.operations") }}</RouterLink>
        <RouterLink v-if="session.role === 'admin'" to="/admin">{{ t("app.userManagement") }}</RouterLink>
      </nav>

      <div class="account">
        <button class="language-button" type="button" :aria-label="t('app.switchLanguage')" @click="toggleLocale">
          {{ locale === "zh-CN" ? "EN" : "中" }}
        </button>
        <template v-if="session.authenticated">
          <div ref="accountMenuRoot" class="account-menu">
            <button ref="accountTrigger" class="account-trigger" type="button" :aria-label="t('app.accountMenu', { name: session.username })" aria-haspopup="menu" :aria-expanded="accountMenuOpen" aria-controls="account-menu-list" @click="toggleAccountMenu" @keydown.down.prevent="focusAccountItem(0)" @keydown.up.prevent="focusAccountItem(-1)">
              <User /><span>{{ session.username }}</span><ArrowDown class="account-chevron" />
            </button>
            <div v-if="accountMenuOpen" id="account-menu-list" class="account-menu-list" role="menu" @keydown="onAccountMenuKeydown">
              <RouterLink to="/settings" role="menuitem" @click="accountMenuOpen = false">{{ t("app.settings") }}</RouterLink>
              <button type="button" role="menuitem" @click="logout">{{ t("app.logout") }}</button>
            </div>
          </div>
        </template>
        <template v-else>
          <RouterLink class="register-link" to="/register">{{ t("app.register") }}</RouterLink>
          <RouterLink class="login-button" to="/login">{{ t("app.login") }}</RouterLink>
        </template>
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
import { ArrowDown, Menu, User } from "@element-plus/icons-vue";
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, RouterView, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";

import { LOCALE_KEY, type Locale } from "@/i18n";
import { useSessionStore } from "@/stores/session";

const menuOpen = ref(false);
const accountMenuOpen = ref(false);
const accountMenuRoot = ref<HTMLElement | null>(null);
const accountTrigger = ref<HTMLButtonElement | null>(null);
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

watch(() => router.currentRoute.value.fullPath, () => { accountMenuOpen.value = false; });

function closeAccountMenu(event: PointerEvent): void {
  if (accountMenuRoot.value && !accountMenuRoot.value.contains(event.target as Node)) accountMenuOpen.value = false;
}

function onEscape(event: KeyboardEvent): void {
  if (event.key === "Escape" && accountMenuOpen.value) {
    accountMenuOpen.value = false;
    accountTrigger.value?.focus();
  }
}

onMounted(() => {
  document.addEventListener("pointerdown", closeAccountMenu);
  document.addEventListener("keydown", onEscape);
});
onBeforeUnmount(() => {
  document.removeEventListener("pointerdown", closeAccountMenu);
  document.removeEventListener("keydown", onEscape);
});

function toggleAccountMenu(): void {
  accountMenuOpen.value = !accountMenuOpen.value;
  menuOpen.value = false;
}

async function focusAccountItem(index: number): Promise<void> {
  accountMenuOpen.value = true;
  menuOpen.value = false;
  await nextTick();
  const items = accountMenuRoot.value?.querySelectorAll<HTMLElement>('[role="menuitem"]');
  items?.[index < 0 ? items.length - 1 : index]?.focus();
}

function onAccountMenuKeydown(event: KeyboardEvent): void {
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
  const items = [...(accountMenuRoot.value?.querySelectorAll<HTMLElement>('[role="menuitem"]') || [])];
  const current = items.indexOf(document.activeElement as HTMLElement);
  if (!items.length || current < 0) return;
  event.preventDefault();
  items[(current + (event.key === "ArrowDown" ? 1 : items.length - 1)) % items.length].focus();
}

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
  accountMenuOpen.value = false;
  session.signOut();
  await router.push("/");
}
</script>
