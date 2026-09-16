<template>
  <section class="page-heading"><div><h1>{{ t("settings.title") }}</h1><p>{{ t("settings.hint", { name: session.username }) }}</p></div></section>
  <section class="content-panel settings-panel"><div class="section-heading"><h2>{{ t("settings.changePassword") }}</h2></div>
    <form class="project-form" @submit.prevent="submit"><label class="field required-field"><span>{{ t("settings.currentPassword") }}</span><input v-model="currentPassword" type="password" autocomplete="current-password" required /></label><label class="field required-field"><span>{{ t("settings.newPassword") }}</span><input v-model="newPassword" type="password" autocomplete="new-password" required /></label><p class="form-hint">{{ t("auth.passwordRule") }}</p><label class="field required-field"><span>{{ t("auth.confirmPassword") }}</span><input v-model="confirmPassword" type="password" autocomplete="new-password" required /></label><div v-if="errorKey" class="form-error" role="alert"><WarningFilled />{{ t(errorKey) }}</div><div v-if="saved" class="success-banner" role="status"><CircleCheckFilled />{{ t("settings.saved") }}</div><div class="modal-actions"><button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("common.saving") : t("common.save") }}</button></div></form>
  </section>
</template>

<script setup lang="ts">
import { CircleCheckFilled, WarningFilled } from "@element-plus/icons-vue";
import { ref } from "vue";
import { useI18n } from "vue-i18n";
import { ApiError, errorTranslationKey, request } from "@/api";
import { useSessionStore } from "@/stores/session";

const { t } = useI18n();
const session = useSessionStore();
const currentPassword = ref("");
const newPassword = ref("");
const confirmPassword = ref("");
const saving = ref(false);
const saved = ref(false);
const errorKey = ref("");

async function submit(): Promise<void> {
  errorKey.value = "";
  saved.value = false;
  const length = Array.from(newPassword.value).length;
  if (length < 12 || length > 128) { errorKey.value = "auth.invalidPassword"; return; }
  if (newPassword.value !== confirmPassword.value) { errorKey.value = "auth.passwordMismatch"; return; }
  saving.value = true;
  try {
    await request<void>(`/auth/users/${encodeURIComponent(session.username)}/password`, { method: "PUT", body: JSON.stringify({ currentPassword: currentPassword.value, newPassword: newPassword.value }) });
    currentPassword.value = "";
    newPassword.value = "";
    confirmPassword.value = "";
    saved.value = true;
  } catch (error) { errorKey.value = error instanceof ApiError && error.status === 401 ? "settings.wrongPassword" : errorTranslationKey(error, "settings.saveFailed"); }
  finally { saving.value = false; }
}
</script>
