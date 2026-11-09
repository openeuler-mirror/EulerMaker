<template>
  <section class="auth-layout">
    <div class="auth-intro">
      <span class="eyebrow">{{ t("auth.eyebrow") }}</span>
      <h1>{{ t("auth.registerHeading") }}</h1>
      <p>{{ t("auth.registerDescription") }}</p>
      <div class="auth-feature"><Lock /><span><strong>{{ t("auth.unifiedAccess") }}</strong><small>{{ t("auth.unifiedAccessHint") }}</small></span></div>
    </div>

    <div v-if="!loginMethods.methods" class="auth-card" aria-live="polite">
      <p>{{ t(loginMethods.failed ? "oauth.unavailable" : "oauth.loading") }}</p>
      <button v-if="loginMethods.failed" class="submit-button" type="button" @click="loginMethods.load()">{{ t("oauth.retry") }}</button>
    </div>
    <div v-else-if="!loginMethods.methods.registrationEnabled" class="auth-card">
      <p>{{ t("oauth.registrationViaLogin") }}</p>
      <RouterLink class="submit-button auth-action-link" :to="loginDestination">{{ t("app.login") }}</RouterLink>
    </div>
    <div v-else-if="registered" class="auth-card" role="status">
      <div class="auth-card-heading"><img src="@/assets/eulermaker-logo.svg" alt="" /><div><h2>{{ t("auth.registerSuccess") }}</h2><p>{{ t("auth.registerSuccessHint") }}</p></div></div>
      <RouterLink class="submit-button auth-action-link" :to="loginDestination">{{ t("app.login") }}</RouterLink>
    </div>

    <form v-else class="auth-card" @submit.prevent="submit">
      <div class="auth-card-heading"><img src="@/assets/eulermaker-logo.svg" alt="" /><div><h2>{{ t("auth.registerTitle") }}</h2><p>{{ t("auth.registerHint") }}</p></div></div>
      <label><span>{{ t("auth.username") }}</span><input v-model.trim="username" autocomplete="username" required maxlength="63" :placeholder="t('auth.usernamePlaceholder')" /></label>
      <p class="field-hint">{{ t("auth.usernameRule") }}</p>
      <label><span>{{ t("auth.displayName") }}</span><input v-model.trim="displayName" autocomplete="nickname" :placeholder="t('auth.displayNamePlaceholder')" /></label>
      <label><span>{{ t("auth.email") }}</span><input v-model.trim="email" type="email" autocomplete="email" :placeholder="t('auth.emailPlaceholder')" /></label>
      <label><span>{{ t("auth.password") }}</span><input v-model="password" type="password" autocomplete="new-password" required :placeholder="t('auth.passwordPlaceholder')" /></label>
      <p class="field-hint">{{ t("auth.passwordRule") }}</p>
      <label><span>{{ t("auth.confirmPassword") }}</span><input v-model="confirmPassword" type="password" autocomplete="new-password" required :placeholder="t('auth.confirmPasswordPlaceholder')" /></label>
      <div v-if="error" class="form-error" role="alert"><WarningFilled />{{ error }}</div>
      <button class="submit-button" type="submit" :disabled="submitting">{{ submitting ? t("auth.registering") : t("app.register") }}</button>
      <RouterLink class="guest-link" :to="loginDestination">{{ t("auth.haveAccount") }} {{ t("app.login") }}</RouterLink>
    </form>
  </section>
</template>

<script setup lang="ts">
import { Lock, WarningFilled } from "@element-plus/icons-vue";
import { computed, onMounted, ref } from "vue";
import { RouterLink, useRoute } from "vue-router";
import { useI18n } from "vue-i18n";

import { ApiError, errorTranslationKey, registerUser } from "@/api";
import { useLoginMethodsStore } from "@/stores/loginMethods";

const loginMethods = useLoginMethodsStore();
onMounted(() => { void loginMethods.load(); });

const route = useRoute();
const { t } = useI18n();
const username = ref("");
const displayName = ref("");
const email = ref("");
const password = ref("");
const confirmPassword = ref("");
const errorKey = ref("");
const submitting = ref(false);
const registered = ref(false);
const error = computed(() => (errorKey.value ? t(errorKey.value) : ""));
const loginDestination = computed(() => ({
  name: "login",
  query: {
    username: username.value || undefined,
    redirect: typeof route.query.redirect === "string" ? route.query.redirect : undefined,
  },
}));

async function submit(): Promise<void> {
  if (!loginMethods.methods?.registrationEnabled || submitting.value) return;
  errorKey.value = "";
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(username.value)) {
    errorKey.value = "auth.invalidUsername";
    return;
  }
  const passwordLength = Array.from(password.value).length;
  if (passwordLength < 12 || passwordLength > 128) {
    errorKey.value = "auth.invalidPassword";
    return;
  }
  if (password.value !== confirmPassword.value) {
    errorKey.value = "auth.passwordMismatch";
    return;
  }

  submitting.value = true;
  try {
    await registerUser({ username: username.value, password: password.value, displayName: displayName.value, email: email.value });
    password.value = "";
    confirmPassword.value = "";
    registered.value = true;
  } catch (reason) {
    errorKey.value = reason instanceof ApiError && reason.status === 409
      ? "auth.usernameTaken"
      : errorTranslationKey(reason, "auth.registerFailed");
  } finally {
    submitting.value = false;
  }
}
</script>
