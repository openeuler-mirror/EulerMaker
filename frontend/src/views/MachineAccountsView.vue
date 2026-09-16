<template>
  <section class="page-heading"><div><p>{{ t("admin.machineHint") }}</p></div><div class="page-actions"><button class="primary-button" type="button" @click="createOpen = true">{{ t("admin.createMachine") }}</button><button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="loading" @click="reload"><Refresh /></button></div></section>
  <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
  <section class="content-panel">
    <div v-if="loading" class="skeleton-list" :aria-label="t('admin.loadingMachines')"><span v-for="item in 5" :key="item"></span></div>
    <div v-else-if="loadError" class="inline-error" role="alert"><WarningFilled />{{ t(loadError) }}<button type="button" @click="loadAccounts">{{ t("common.reload") }}</button></div>
    <EmptyState v-else-if="!accounts.length" :title="t('admin.noMachines')" :description="t('admin.noMachinesHint')" />
    <div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("admin.machineName") }}</th><th>{{ t("admin.tokenTTL") }}</th><th>{{ t("projects.createdAt") }}</th><th>{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="account in accounts" :key="account.metadata?.name"><td><strong>{{ account.metadata?.name }}</strong></td><td>{{ account.spec?.tokenTTLSeconds || 3600 }} s</td><td>{{ formatDate(account.metadata?.creationTimestamp) }}</td><td><button class="text-button danger-link" type="button" @click="openDelete(account)">{{ t("common.remove") }}</button></td></tr></tbody></table></div>
    <div v-if="!loading && !loadError" class="table-footer"><span>{{ t("common.count", { count: accounts.length }) }}</span><div class="pagination-row"><button class="page-button arrow-button" type="button" :disabled="page === 1" :aria-label="t('common.previous')" @click="changePage(-1)"><ArrowLeft /></button><strong>{{ page }}</strong><button class="page-button arrow-button" type="button" :disabled="!nextToken" :aria-label="t('common.next')" @click="changePage(1)"><ArrowRight /></button></div></div>
  </section>

  <ModalDialog v-if="createOpen" title-id="create-machine-title" :title="t('admin.createMachine')" :close-label="t('common.close')" @close="closeCreate">
    <form class="project-form" @submit.prevent="createAccount"><p class="form-hint">{{ t("admin.machineCreateHint") }}</p><label class="field required-field"><span>{{ t("admin.machineName") }}</span><input v-model.trim="name" required maxlength="63" autocomplete="off" :placeholder="t('admin.machineNamePlaceholder')" /></label><label class="field required-field"><span>{{ t("admin.tokenTTL") }}</span><input v-model.number="ttl" type="number" min="300" max="86400" required /></label><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeCreate">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("admin.creating") : t("admin.createMachine") }}</button></div></form>
  </ModalDialog>

  <ModalDialog v-if="secret" title-id="machine-secret-title" :title="t('admin.secretTitle')" :close-label="t('common.close')" @close="closeSecret"><p class="machine-secret-warning">{{ t("admin.secretWarning") }}</p><label class="field"><span>{{ t("admin.machineName") }}</span><input :value="createdName" readonly /></label><label class="field machine-secret-field"><span>{{ t("admin.clientSecret") }}</span><textarea :value="secret" readonly rows="4" spellcheck="false" @focus="($event.target as HTMLTextAreaElement).select()"></textarea></label><div class="modal-actions"><button class="primary-button" type="button" @click="closeSecret">{{ t("admin.secretSaved") }}</button></div></ModalDialog>

  <ModalDialog v-if="deleting" title-id="delete-machine-title" :title="t('admin.deleteMachine', { name: deleting.metadata?.name })" :close-label="t('common.close')" @close="closeDelete"><form class="project-form" @submit.prevent="deleteAccount"><p class="form-hint">{{ t("admin.typeNameHint", { name: deleting.metadata?.name }) }}</p><label class="field"><span>{{ t("admin.machineName") }}</span><input v-model.trim="confirmName" autocomplete="off" required /></label><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeDelete">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="submit" :disabled="saving || confirmName !== deleting.metadata?.name">{{ t("common.remove") }}</button></div></form></ModalDialog>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, CircleCheckFilled, Refresh, WarningFilled } from "@element-plus/icons-vue";
import { onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ApiError, errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import type { MachineAccount } from "@/types";

const { t } = useI18n();
const accounts = ref<MachineAccount[]>([]);
const loading = ref(true);
const loadError = ref("");
const dialogError = ref("");
const success = ref("");
const page = ref(1);
const tokens = ref([""]);
const nextToken = ref("");
const createOpen = ref(false);
const name = ref("");
const ttl = ref(3600);
const saving = ref(false);
const secret = ref("");
const createdName = ref("");
const deleting = ref<MachineAccount | null>(null);
const confirmName = ref("");

onMounted(() => { void loadAccounts(); });
onBeforeUnmount(() => { secret.value = ""; });
async function loadAccounts(): Promise<void> {
  loading.value = true;
  loadError.value = "";
  try {
    const query = new URLSearchParams({ limit: "50" });
    const token = tokens.value[page.value - 1];
    if (token) query.set("continue", token);
    const result = await list<MachineAccount>(`/apis/iam.ebs/v1/machineaccounts?${query}`);
    accounts.value = result.items;
    nextToken.value = result.next;
    if (result.next) tokens.value[page.value] = result.next;
  } catch (error) { loadError.value = errorTranslationKey(error, "admin.loadMachinesFailed"); }
  finally { loading.value = false; }
}
function reload(): void { page.value = 1; tokens.value = [""]; void loadAccounts(); }
function changePage(direction: number): void { page.value += direction; void loadAccounts(); }
function closeCreate(): void { if (!saving.value) createOpen.value = false; }
function closeSecret(): void { secret.value = ""; createdName.value = ""; }
function openDelete(account: MachineAccount): void { deleting.value = account; confirmName.value = ""; dialogError.value = ""; success.value = ""; }
function closeDelete(): void { if (!saving.value) deleting.value = null; }
async function createAccount(): Promise<void> {
  if (saving.value) return;
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(name.value) || !Number.isInteger(ttl.value) || ttl.value < 300 || ttl.value > 86400) { dialogError.value = "admin.invalidMachine"; return; }
  saving.value = true;
  dialogError.value = "";
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const generatedSecret = btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  try {
    await request<{ name: string }>("/auth/machineaccounts", { method: "POST", body: JSON.stringify({ name: name.value, clientSecret: generatedSecret, tokenTTLSeconds: ttl.value }) });
    createdName.value = name.value;
    secret.value = generatedSecret;
    name.value = "";
    createOpen.value = false;
    reload();
  } catch (error) { dialogError.value = error instanceof ApiError && error.status === 409 ? "admin.machineExists" : errorTranslationKey(error, "admin.createMachineFailed"); }
  finally { saving.value = false; }
}
async function deleteAccount(): Promise<void> {
  const accountName = deleting.value?.metadata?.name;
  if (!accountName || confirmName.value !== accountName || saving.value) return;
  saving.value = true;
  dialogError.value = "";
  try {
    await request<unknown>(`/apis/iam.ebs/v1/machineaccounts/${encodeURIComponent(accountName)}`, { method: "DELETE" });
    success.value = t("admin.machineDeleted", { name: accountName });
    deleting.value = null;
    reload();
  } catch (error) { dialogError.value = errorTranslationKey(error, "admin.deleteMachineFailed"); }
  finally { saving.value = false; }
}
function formatDate(value?: string): string {
  if (!value) return t("common.emptyValue");
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return t("common.emptyValue");
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
</script>
