<template>
  <section class="content-panel credentials-panel" aria-labelledby="credentials-heading">
    <div class="section-heading">
      <h2 id="credentials-heading">{{ t("credentials.title") }} <small v-if="loaded">{{ activeItems.length }}/5</small></h2>
      <div class="credential-actions">
        <button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="busy || loading" @click="load"><Refresh /></button>
        <button class="primary-button" type="button" :disabled="!canCreate || createOpen || !!secret || !!revoking" @click="openCreate">{{ t("credentials.create") }}</button>
      </div>
    </div>
    <p class="form-hint">{{ t("credentials.hint") }}</p>
    <p v-if="session.role === 'ops' || session.role === 'admin'" class="credential-warning">{{ t("credentials.privilegedHint") }}</p>
    <p v-if="loaded && activeItems.length >= 5" class="form-hint">{{ t("credentials.limit") }}</p>
    <p v-if="loadError" class="form-error" role="alert">{{ t(loadError) }}</p>
    <p v-if="uncertain" class="credential-warning" role="alert">
      {{ t("credentials.unknownResult") }}
      <button class="text-button" type="button" :disabled="!loaded || loading || busy" @click="uncertain = false">{{ t("credentials.reviewed") }}</button>
    </p>
    <p v-if="notice" class="success-banner" role="status">{{ t(notice) }}</p>
    <p v-if="loading" role="status">{{ t("credentials.loading") }}</p>
    <div v-else-if="loaded" class="project-table-wrap">
      <table class="project-table">
        <thead><tr><th>{{ t("credentials.name") }}</th><th>{{ t("credentials.createdAt") }}</th><th>{{ t("credentials.expiresAt") }}</th><th>{{ t("credentials.actions") }}</th></tr></thead>
        <tbody>
          <tr v-for="item in activeItems" :key="item.id">
            <td>{{ item.name }}</td><td>{{ formatDate(item.createdAt) }}</td><td>{{ formatDate(item.expiresAt) }}</td>
            <td><button class="text-button danger-link" type="button" :disabled="busy" @click="openRevoke(item)">{{ t("credentials.revoke") }}</button></td>
          </tr>
          <tr v-if="!activeItems.length"><td colspan="4">{{ t("credentials.empty") }}</td></tr>
        </tbody>
      </table>
    </div>
  </section>

  <ModalDialog v-if="createOpen" title-id="credential-create" :title="t('credentials.create')" :close-label="t('common.close')" @close="closeCreate">
    <form class="project-form" @submit.prevent="create">
      <label class="field required-field"><span>{{ t("credentials.name") }}</span><input v-model="name" required maxlength="64" :disabled="busy" autocomplete="off" /></label>
      <label class="field required-field"><span>{{ t("credentials.days") }}</span><input v-model.number="days" type="number" min="1" max="90" step="1" required :disabled="busy" /></label>
      <p class="form-hint">{{ t("credentials.hint") }}</p>
      <p v-if="session.role === 'ops' || session.role === 'admin'" class="credential-warning">{{ t("credentials.privilegedHint") }}</p>
      <p v-if="dialogError" class="form-error" role="alert">{{ t(dialogError) }}</p>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="busy" @click="closeCreate">{{ t("common.cancel") }}</button>
        <button class="primary-button" type="submit" :disabled="busy || !canCreate">{{ t(busy ? "common.saving" : "credentials.create") }}</button>
      </div>
    </form>
  </ModalDialog>

  <ModalDialog v-if="secret" title-id="credential-secret" :title="t('credentials.created')" :close-label="t('common.close')" @close="closeSecret">
    <div class="project-form">
      <p class="credential-warning">{{ t("credentials.oneTime") }}</p>
      <label class="field"><span>{{ t("credentials.token") }}</span><textarea :value="secret" readonly rows="4" spellcheck="false" autocomplete="off" /></label>
      <p class="form-hint">{{ t("credentials.cliHint") }}</p>
      <code>ebsctl login &lt;gateway&gt; --token-stdin</code>
      <p v-if="copyMessage" role="status">{{ t(copyMessage) }}</p>
      <div class="modal-actions">
        <button class="secondary-button" type="button" @click="copySecret">{{ t("credentials.copy") }}</button>
        <button class="primary-button" type="button" @click="closeSecret">{{ t("credentials.saved") }}</button>
      </div>
    </div>
  </ModalDialog>

  <ModalDialog v-if="revoking" title-id="credential-revoke" :title="t('credentials.revoke')" :close-label="t('common.close')" @close="closeRevoke">
    <div class="project-form">
      <p>{{ t("credentials.revokeConfirm", { name: revoking.name }) }}</p>
      <p class="form-hint">{{ t("credentials.revokeImpact") }}</p>
      <p v-if="dialogError" class="form-error" role="alert">{{ t(dialogError) }}</p>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="busy" @click="closeRevoke">{{ t("common.cancel") }}</button>
        <button class="primary-button" type="button" :disabled="busy" @click="revoke">{{ t("credentials.revoke") }}</button>
      </div>
    </div>
  </ModalDialog>
</template>

<script setup lang="ts">
import { Refresh } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { useRouter } from "vue-router";
import { ApiError, errorTranslationKey, request } from "@/api";
import { useSessionStore } from "@/stores/session";
import ModalDialog from "@/components/ModalDialog.vue";

interface PersonalCredential { id: string; name: string; createdAt: string; expiresAt: string }

const { t, locale } = useI18n();
const session = useSessionStore();
const router = useRouter();
const items = ref<PersonalCredential[]>([]);
const now = ref(Date.now());
const loaded = ref(false);
const loading = ref(false);
const busy = ref(false);
const loadError = ref("");
const dialogError = ref("");
const notice = ref("");
const createOpen = ref(false);
const name = ref("");
const days = ref(90);
const uncertain = ref(false);
const secret = ref("");
const copyMessage = ref("");
const revoking = ref<PersonalCredential | null>(null);
const activeItems = computed(() => items.value.filter(item => Date.parse(item.expiresAt) > now.value));
const canCreate = computed(() => loaded.value && !loading.value && !busy.value && !uncertain.value && activeItems.value.length < 5);
let epoch = 0;
let lifetime = new AbortController();
let timer: ReturnType<typeof setInterval> | undefined;

function options(init: RequestInit = {}): RequestInit {
  return { ...init, cache: "no-store", credentials: "omit", signal: AbortSignal.any([lifetime.signal, AbortSignal.timeout(15000)]) };
}

function errorKey(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    if (error.status === 401) {
      session.signOut();
      void router.replace({ name: "login", query: { redirect: "/settings" } });
      return "errors.unauthorized";
    }
    if (error.code === "CredentialLimitExceeded") return "credentials.limit";
    if (error.code === "CredentialConflict") return "credentials.conflict";
  }
  return errorTranslationKey(error, fallback);
}

async function load(): Promise<void> {
  if (loading.value || !session.authenticated) return;
  const current = epoch;
  loading.value = true;
  loaded.value = false;
  loadError.value = "";
  try {
    const result = await request<{ items: PersonalCredential[] }>("/auth/credentials", options());
    if (current !== epoch) return;
    // Keep only metadata, even if an unexpected response includes secret fields.
    items.value = result.items.map(({ id, name, createdAt, expiresAt }) => ({ id, name, createdAt, expiresAt }));
    now.value = Date.now();
    loaded.value = true;
  } catch (error) {
    if (current === epoch) loadError.value = errorKey(error, "credentials.loadFailed");
  } finally { if (current === epoch) loading.value = false; }
}

function openCreate(): void {
  if (!canCreate.value || secret.value || revoking.value) return;
  dialogError.value = "";
  notice.value = "";
  createOpen.value = true;
}

function closeCreate(): void { if (!busy.value) createOpen.value = false; }
function closeSecret(): void { secret.value = ""; copyMessage.value = ""; }
function closeRevoke(): void { if (!busy.value) revoking.value = null; }

async function create(): Promise<void> {
  if (!createOpen.value || !canCreate.value) return;
  const trimmed = name.value.trim();
  if (!trimmed || Array.from(trimmed).length > 64 || !Number.isInteger(days.value) || days.value < 1 || days.value > 90) {
    dialogError.value = "credentials.invalidInput";
    return;
  }
  const current = epoch;
  busy.value = true;
  dialogError.value = "";
  try {
    const result = await request<PersonalCredential & { token: string }>("/auth/credentials", options({
      method: "POST", body: JSON.stringify({ name: trimmed, expiresInDays: days.value }),
    }));
    if (current !== epoch) return;
    if (!result.token) throw new Error("missing credential");
    secret.value = result.token;
    createOpen.value = false;
    name.value = "";
    days.value = 90;
    await load();
  } catch (error) {
    if (current !== epoch) return;
    uncertain.value = !(error instanceof ApiError) || error.status >= 500;
    dialogError.value = uncertain.value ? "credentials.unknownResult" : errorKey(error, "credentials.createFailed");
    if (current === epoch && (uncertain.value || (error instanceof ApiError && error.status === 409))) await load();
  } finally { if (current === epoch) busy.value = false; }
}

function openRevoke(item: PersonalCredential): void {
  if (busy.value || createOpen.value || secret.value) return;
  revoking.value = item;
  dialogError.value = "";
  notice.value = "";
}

async function revoke(): Promise<void> {
  if (!revoking.value || busy.value) return;
  const current = epoch;
  busy.value = true;
  dialogError.value = "";
  try {
    await request<void>(`/auth/credentials/${encodeURIComponent(revoking.value.id)}`, options({ method: "DELETE" }));
    if (current !== epoch) return;
    revoking.value = null;
    notice.value = "credentials.revoked";
    await load();
  } catch (error) {
    if (current === epoch) dialogError.value = errorKey(error, "credentials.revokeFailed");
  } finally { if (current === epoch) busy.value = false; }
}

async function copySecret(): Promise<void> {
  const value = secret.value;
  const current = epoch;
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
    if (current === epoch && secret.value === value) copyMessage.value = "credentials.copied";
  } catch {
    if (current === epoch && secret.value === value) copyMessage.value = "credentials.copyFailed";
  }
}

function formatDate(value: string): string { return new Date(value).toLocaleString(locale.value); }

function clear(): void {
  epoch++;
  lifetime.abort();
  lifetime = new AbortController();
  closeSecret();
  items.value = [];
  loaded.value = loading.value = busy.value = createOpen.value = uncertain.value = false;
  revoking.value = null;
  name.value = loadError.value = dialogError.value = notice.value = "";
  days.value = 90;
}

watch(() => [session.username, session.authenticated], () => {
  clear();
  if (session.authenticated) void load();
}, { flush: "sync" });
onMounted(() => {
  void load();
  window.addEventListener("pagehide", clear);
  timer = setInterval(() => { now.value = Date.now(); }, 60000);
});
onBeforeUnmount(() => { window.removeEventListener("pagehide", clear); clearInterval(timer); clear(); });
</script>

<style scoped>
.credentials-panel { margin-top: 24px; }
.credential-actions { display: flex; align-items: center; gap: 12px; }
.section-heading { flex-wrap: wrap; gap: 12px; }
h2 small { color: var(--muted); font-size: 14px; font-weight: 400; }
.credential-warning { color: #946200; font-size: 14px; line-height: 1.7; }
.project-table-wrap { margin-top: 16px; }
td:first-child { overflow-wrap: anywhere; min-width: 120px; }
textarea { overflow-wrap: anywhere; font-family: monospace; }
code { overflow-wrap: anywhere; }
</style>
