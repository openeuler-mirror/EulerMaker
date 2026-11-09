<template>
  <section class="oauth-layout">
    <div class="auth-card" aria-live="polite">
      <div class="auth-card-heading"><img src="@/assets/eulermaker-logo.svg" alt="" /><h2>{{ t("oauth.callbackTitle") }}</h2></div>
      <p v-if="!errorKey">{{ t("oauth.verifying") }}</p>
      <div v-else class="form-error" role="alert">{{ t(errorKey) }}</div>
      <RouterLink v-if="errorKey" class="submit-button auth-action-link" to="/login">{{ t("oauth.restart") }}</RouterLink>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from "vue";
import { RouterLink, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import { beginRegistration, clearRegistration, oauthErrorKey, oauthRequest, safeReturnTo, takeOAuthCallback, type OAuthLogin, type RegistrationContext } from "@/oauth";
import { useSessionStore } from "@/stores/session";

const { t } = useI18n();
const router = useRouter();
const session = useSessionStore();
const errorKey = ref("");
let disposed = false;
onBeforeUnmount(() => { disposed = true; });

onMounted(async () => {
  try {
    const { provider, body } = takeOAuthCallback();
    clearRegistration();
    const result = await oauthRequest<OAuthLogin | RegistrationContext>(`/auth/oauth/${provider}/callback`, body);
    if (disposed) return;
    if ("registrationRequired" in result) {
      beginRegistration(result);
      await router.replace({ name: "oauth-register" });
    } else {
      await session.completeOAuth(result.token);
      if (disposed) return;
      await router.replace(safeReturnTo(result.returnTo));
    }
  } catch (error) { errorKey.value = oauthErrorKey(error); }
});
</script>
