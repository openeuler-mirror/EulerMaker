<template>
  <section class="auth-layout">
    <div class="auth-intro">
      <span class="eyebrow">{{ t("auth.eyebrow") }}</span>
      <h1>{{ t("auth.title") }}</h1>
      <p>{{ t("auth.description") }}</p>
      <div class="auth-feature"><Lock /><span><strong>{{ t("auth.unifiedAccess") }}</strong><small>{{ t("auth.unifiedAccessHint") }}</small></span></div>
      <div class="auth-feature"><Timer /><span><strong>{{ t("auth.sessionOnly") }}</strong><small>{{ t("auth.sessionOnlyHint") }}</small></span></div>
    </div>

    <form class="auth-card" @submit.prevent="submit">
      <div class="auth-card-heading"><img src="@/assets/eulermaker-logo.svg" alt="" /><div><h2>{{ t("auth.loginTitle") }}</h2><p>{{ t("auth.loginHint") }}</p></div></div>
      <label><span>{{ t("auth.username") }}</span><input v-model.trim="username" autocomplete="username" required maxlength="63" :placeholder="t('auth.usernamePlaceholder')" /></label>
      <label><span>{{ t("auth.password") }}</span><input v-model="password" type="password" autocomplete="current-password" required maxlength="128" :placeholder="t('auth.passwordPlaceholder')" /></label>
      <RouterLink class="auth-switch-link" :to="registerDestination">{{ t("auth.noAccount") }} {{ t("app.register") }}</RouterLink>
      <div v-if="error" class="form-error" role="alert"><WarningFilled />{{ error }}</div>
      <button class="submit-button" type="submit" :disabled="submitting">{{ submitting ? t("auth.submitting") : t("app.login") }}</button>
      <RouterLink class="guest-link" to="/projects">{{ t("auth.browseAsGuest") }}</RouterLink>
    </form>
  </section>
</template>

<script setup lang="ts">
import { Lock, Timer, WarningFilled } from "@element-plus/icons-vue";
import { computed, ref } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";

import { errorTranslationKey } from "@/api";
import { useSessionStore } from "@/stores/session";

const route = useRoute();
const username = ref(typeof route.query.username === "string" ? route.query.username : "");
const password = ref("");
const errorKey = ref("");
const submitting = ref(false);
const session = useSessionStore();
const { t } = useI18n();
const error = computed(() => (errorKey.value ? t(errorKey.value) : ""));
const router = useRouter();
const registerDestination = computed(() => ({ name: "register", query: typeof route.query.redirect === "string" ? { redirect: route.query.redirect } : {} }));

async function submit(): Promise<void> {
  submitting.value = true;
  errorKey.value = "";
  try {
    await session.signIn(username.value, password.value);
    const redirect =
      typeof route.query.redirect === "string" && route.query.redirect.startsWith("/") && !route.query.redirect.startsWith("//")
        ? route.query.redirect
        : "/";
    await router.push(redirect);
  } catch (reason) {
    errorKey.value = errorTranslationKey(reason, "errors.loginFailed");
  } finally {
    submitting.value = false;
  }
}
</script>
