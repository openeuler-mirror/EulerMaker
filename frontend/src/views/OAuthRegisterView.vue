<template>
  <section class="oauth-layout">
    <form v-if="draft" class="auth-card" @submit.prevent="submit">
      <div class="auth-card-heading"><img src="@/assets/eulermaker-logo.svg" alt="" /><div><h2>{{ t("oauth.registerTitle") }}</h2><p>{{ t("oauth.registerHint") }}</p></div></div>
      <label><span>{{ t("auth.username") }}</span><input v-model="draft.username" :disabled="locked" autocomplete="username" maxlength="63" required /></label>
      <p class="field-hint">{{ t("auth.usernameRule") }}</p>
      <label><span>{{ t("auth.displayName") }}</span><input v-model="draft.displayName" :disabled="locked" autocomplete="nickname" /></label>
      <label><span>{{ t("auth.email") }}</span><input v-model="draft.email" :disabled="locked" type="email" autocomplete="email" /></label>
      <p class="field-hint">{{ t("oauth.expiresAt", { time: new Date(draft.expiresAt).toLocaleTimeString() }) }}</p>
      <p v-if="draft.submitted && !restartRequired" role="status">{{ t("oauth.unknownResult") }}</p>
      <div v-if="errorKey" class="form-error" role="alert">{{ t(errorKey) }}</div>
      <button v-if="!draft.submitted && !restartRequired" class="submit-button" type="submit" :disabled="busy">{{ t(busy ? "auth.registering" : "oauth.completeRegistration") }}</button>
      <button v-if="draft.submitted && !restartRequired" class="submit-button" type="button" :disabled="busy" @click="confirm(false)">{{ t(busy ? "oauth.confirming" : "oauth.confirm") }}</button>
      <RouterLink class="guest-link" to="/login" @click="cancel">{{ t(restartRequired ? "oauth.restart" : "oauth.cancel") }}</RouterLink>
    </form>
    <div v-else class="auth-card">
      <h2>{{ t("oauth.registerTitle") }}</h2>
      <p role="alert">{{ t("oauth.expired") }}</p>
      <RouterLink class="submit-button auth-action-link" to="/login">{{ t("oauth.restart") }}</RouterLink>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import { clearRegistration, oauthErrorKey, oauthRequest, readRegistration, registrationErrorAction, safeReturnTo, saveRegistration, type OAuthLogin } from "@/oauth";
import { useSessionStore } from "@/stores/session";

const { t } = useI18n();
const router = useRouter();
const session = useSessionStore();
const draft = ref(readRegistration());
const busy = ref(false);
const restartRequired = ref(false);
const errorKey = ref("");
const locked = computed(() => busy.value || draft.value?.submitted || restartRequired.value);
let disposed = false;
let timer: ReturnType<typeof setTimeout> | undefined;
let expiryTimer: ReturnType<typeof setTimeout> | undefined;
let confirmations = 0;

function cancel(): void {
  clearTimeout(timer);
  clearRegistration();
}
function expire(): boolean {
  if (!draft.value || Date.parse(draft.value.expiresAt) <= Date.now()) {
    cancel();
    draft.value = null;
    return true;
  }
  const current = readRegistration();
  if (!current || current.registrationID !== draft.value.registrationID) {
    clearTimeout(timer);
    draft.value = null;
    return true;
  }
  return false;
}
watch(draft, value => {
  if (!value || value.submitted || busy.value || restartRequired.value) return;
  try { saveRegistration(value); }
  catch (error) { errorKey.value = oauthErrorKey(error); restartRequired.value = true; }
}, { deep: true, flush: "sync" });

function scheduleConfirmation(): void {
  if (disposed || restartRequired.value || confirmations >= 3 || expire()) return;
  clearTimeout(timer);
  timer = setTimeout(() => { void confirm(true); }, 3000);
}
async function complete(result: OAuthLogin): Promise<void> {
  if (disposed) return;
  await session.completeOAuth(result.token);
  if (disposed) return;
  clearTimeout(timer);
  clearRegistration();
  await router.replace(safeReturnTo(result.returnTo));
}
async function submit(): Promise<void> {
  if (busy.value || restartRequired.value || expire() || !draft.value || draft.value.submitted) return;
  if (!/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(draft.value.username)) {
    errorKey.value = "auth.invalidUsername";
    return;
  }
  busy.value = true;
  errorKey.value = "";
  try {
    // Persist before sending; refresh can only confirm, never silently recreate.
    const submitted = { ...draft.value, submitted: true };
    saveRegistration(submitted);
    draft.value = submitted;
  } catch (error) {
    errorKey.value = oauthErrorKey(error);
    restartRequired.value = true;
    busy.value = false;
    return;
  }
  try {
    const { registrationID, username, displayName, email } = draft.value;
    await complete(await oauthRequest<OAuthLogin>("/auth/oauth/register", { registrationID, username, displayName, email, action: "create" }));
  } catch (error) {
    if (disposed) return;
    errorKey.value = oauthErrorKey(error);
    const action = registrationErrorAction(error);
    if (action === "edit" && draft.value) {
      const editable = { ...draft.value, submitted: false };
      try { saveRegistration(editable); draft.value = editable; }
      catch { errorKey.value = "oauth.storageUnavailable"; restartRequired.value = true; }
    } else if (action === "restart") restartRequired.value = true;
    else scheduleConfirmation();
  } finally { busy.value = false; }
}
async function confirm(automatic: boolean): Promise<void> {
  if (busy.value || restartRequired.value || expire() || !draft.value?.submitted) return;
  clearTimeout(timer);
  if (automatic) confirmations++;
  busy.value = true;
  errorKey.value = "";
  try {
    const { registrationID, username } = draft.value;
    await complete(await oauthRequest<OAuthLogin>("/auth/oauth/register", { registrationID, username, action: "confirm" }));
  } catch (error) {
    if (disposed) return;
    errorKey.value = oauthErrorKey(error);
    if (registrationErrorAction(error) === "restart") restartRequired.value = true;
    else if (automatic) scheduleConfirmation();
  } finally { busy.value = false; }
}
onMounted(() => {
  if (expire() || !draft.value) return;
  expiryTimer = setTimeout(() => { expire(); }, Date.parse(draft.value.expiresAt) - Date.now());
  if (draft.value.submitted) scheduleConfirmation();
});
onBeforeUnmount(() => { disposed = true; clearTimeout(timer); clearTimeout(expiryTimer); });
</script>
