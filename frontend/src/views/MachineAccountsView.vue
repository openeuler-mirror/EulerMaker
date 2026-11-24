<template>
  <section class="page-heading"><div><p>{{ t("admin.machineHint") }}</p></div><div class="page-actions"><button class="primary-button" type="button" @click="openCreate">{{ t("admin.createMachine") }}</button><button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="loading" @click="reload"><Refresh /></button></div></section>
  <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
  <section class="content-panel">
    <div v-if="loading" class="skeleton-list" :aria-label="t('admin.loadingMachines')"><span v-for="item in 5" :key="item"></span></div>
    <div v-else-if="loadError" class="inline-error" role="alert"><WarningFilled />{{ t(loadError) }}<button type="button" @click="loadAccounts">{{ t("common.reload") }}</button></div>
    <div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("admin.machineName") }}</th><th><ColumnMultiFilter v-model="typeFilter" :options="typeOptions" :label="t('admin.machineType')" :all-label="t('admin.allMachineTypes')" /></th><th>{{ t("admin.tokenTTL") }}</th><th>{{ t("projects.createdAt") }}</th><th>{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="account in filteredAccounts" :key="account.metadata?.name"><td><strong>{{ account.metadata?.name }}</strong></td><td>{{ account.spec?.type || "runner" }}</td><td>{{ account.spec?.tokenTTLSeconds || 3600 }} s</td><td>{{ formatDate(account.metadata?.creationTimestamp) }}</td><td><button class="text-button" type="button" @click="openEdit(account)">{{ t("admin.editMachineGrants") }}</button><button class="text-button danger-link" type="button" @click="openDelete(account)">{{ t("common.remove") }}</button></td></tr><tr v-if="!filteredAccounts.length"><td colspan="5"><EmptyState :title="t('admin.noMachines')" :description="t('admin.noMachinesHint')" /></td></tr></tbody></table></div>
    <div v-if="!loading && !loadError" class="table-footer"><span>{{ t("common.count", { count: filteredAccounts.length }) }}</span><div class="pagination-row"><button class="page-button arrow-button" type="button" :disabled="page === 1" :aria-label="t('common.previous')" @click="changePage(-1)"><ArrowLeft /></button><strong>{{ page }}</strong><button class="page-button arrow-button" type="button" :disabled="!nextToken" :aria-label="t('common.next')" @click="changePage(1)"><ArrowRight /></button></div></div>
  </section>

  <ModalDialog v-if="createOpen" title-id="create-machine-title" :title="t(editing ? 'admin.editMachineGrants' : 'admin.createMachine')" :close-label="t('common.close')" @close="closeCreate">
    <form class="project-form" @submit.prevent="editing ? updateAccount() : createAccount()">
      <p class="form-hint">{{ t(editing ? "admin.machineGrantsEditHint" : "admin.machineCreateHint") }}</p>
      <label class="field required-field"><span>{{ t("admin.machineName") }}</span><input v-model.trim="name" required maxlength="63" autocomplete="off" :readonly="!!editing" :disabled="saving" :placeholder="t('admin.machineNamePlaceholder')" /></label>
      <div class="field required-field">
        <span>{{ t("admin.machineType") }}</span>
        <AppSelect v-model="accountType" :options="typeOptions" :label="t('admin.machineType')" :disabled="saving || !!editing" required />
        <small class="form-hint">{{ t("admin.machineTypeHint") }}</small>
        <small v-if="accountType === 'service'" class="form-hint">{{ t("admin.serviceAccountHint") }}</small>
      </div>
      <template v-if="accountType === 'service'">
        <label class="field">
          <span>{{ t("admin.serviceProjects") }}</span>
          <textarea v-model="projectNames" rows="3" :disabled="saving" spellcheck="false" :placeholder="t('admin.serviceProjectsPlaceholder')" aria-describedby="service-projects-hint"></textarea>
          <small id="service-projects-hint" class="form-hint">{{ t("admin.serviceProjectsHint") }}</small>
        </label>
        <fieldset class="target-fieldset" :disabled="saving" aria-describedby="service-permissions-hint">
          <legend>{{ t("admin.servicePermissions") }}</legend>
          <div class="service-permissions">
            <label v-for="option in permissionOptions" :key="option.value">
              <input v-model="permissions" type="checkbox" :value="option.value" />
              <span>{{ option.label }} <code>{{ option.value }}</code></span>
            </label>
          </div>
          <small id="service-permissions-hint" class="form-hint">{{ t("admin.servicePermissionsHint") }}</small>
        </fieldset>
      </template>
      <label class="field required-field"><span>{{ t("admin.tokenTTL") }}</span><input v-model.number="ttl" type="number" min="300" max="86400" :disabled="saving" required /><small v-if="editing" class="form-hint">{{ t("admin.tokenTTLUpdateHint") }}</small></label>
      <div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeCreate">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="saving">{{ t(editing ? (saving ? "admin.savingMachineGrants" : "common.save") : (saving ? "admin.creating" : "admin.createMachine")) }}</button></div>
    </form>
  </ModalDialog>

  <ModalDialog v-if="secret" title-id="machine-secret-title" :title="t('admin.secretTitle')" :close-label="t('common.close')" @close="closeSecret"><p class="machine-secret-warning">{{ t("admin.secretWarning") }}</p><label class="field"><span>{{ t("admin.machineName") }}</span><input :value="createdName" readonly /></label><label class="field machine-secret-field"><span>{{ t("admin.clientSecret") }}</span><textarea :value="secret" readonly rows="4" spellcheck="false" @focus="($event.target as HTMLTextAreaElement).select()"></textarea></label><div class="modal-actions"><button class="primary-button" type="button" @click="closeSecret">{{ t("admin.secretSaved") }}</button></div></ModalDialog>

  <ModalDialog v-if="deleting" title-id="delete-machine-title" :title="t('admin.deleteMachine', { name: deleting.metadata?.name })" :close-label="t('common.close')" @close="closeDelete"><form class="project-form" @submit.prevent="deleteAccount"><p class="form-hint">{{ t("admin.typeNameHint", { name: deleting.metadata?.name }) }}</p><label class="field"><span>{{ t("admin.machineName") }}</span><input v-model.trim="confirmName" autocomplete="off" required /></label><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeDelete">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="submit" :disabled="saving || confirmName !== deleting.metadata?.name">{{ t("common.remove") }}</button></div></form></ModalDialog>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, CircleCheckFilled, Refresh, WarningFilled } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ApiError, errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import AppSelect from "@/components/AppSelect.vue";
import ColumnMultiFilter from "@/components/ColumnMultiFilter.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import type { MachineAccount, MachineAccountPermission, MachineAccountType } from "@/types";

const { t } = useI18n();
const accounts = ref<MachineAccount[]>([]);
const typeFilter = ref<string[]>([]);
const filteredAccounts = computed(() => accounts.value.filter(account =>
  !typeFilter.value.length || typeFilter.value.includes(account.spec?.type || "runner")));
const loading = ref(true);
const loadError = ref("");
const dialogError = ref("");
const success = ref("");
const page = ref(1);
const tokens = ref([""]);
const nextToken = ref("");
const createOpen = ref(false);
const editing = ref<MachineAccount | null>(null);
const name = ref("");
const accountType = ref<MachineAccountType>("runner");
const projectNames = ref("");
const permissions = ref<MachineAccountPermission[]>([]);
const permissionOptions = computed(() => [
  { value: "projects:update", label: t("admin.permissionProjectsUpdate") },
  { value: "builds:create", label: t("admin.permissionBuildsCreate") },
  { value: "builds:abort", label: t("admin.permissionBuildsAbort") },
]);
const typeOptions = computed(() => [
  { value: "runner", label: t("admin.machineTypeRunner") },
  { value: "service", label: t("admin.machineTypeService") },
]);
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
function openCreate(): void {
  editing.value = null;
  name.value = "";
  accountType.value = "runner";
  projectNames.value = "";
  permissions.value = [];
  ttl.value = 3600;
  dialogError.value = "";
  createOpen.value = true;
}
function openEdit(account: MachineAccount): void {
  const type = account.spec?.type || "runner";
  if (type !== "service" && type !== "runner") return;
  openCreate();
  editing.value = JSON.parse(JSON.stringify(account)) as MachineAccount;
  name.value = account.metadata?.name || "";
  accountType.value = type;
  ttl.value = account.spec?.tokenTTLSeconds || 3600;
  projectNames.value = (account.spec?.projects || []).join("\n");
  permissions.value = [...(account.spec?.permissions || [])];
}
function closeCreate(): void { if (!saving.value) createOpen.value = false; }
function closeSecret(): void { secret.value = ""; createdName.value = ""; }
function openDelete(account: MachineAccount): void { deleting.value = account; confirmName.value = ""; dialogError.value = ""; success.value = ""; }
function closeDelete(): void { if (!saving.value) deleting.value = null; }
function serviceGrants(): { projects: string[]; permissions: MachineAccountPermission[] } | null {
  const projects = projectNames.value.split(/[\s,，]+/).filter(Boolean);
  if (projects.some(project => !/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(project)) || new Set(projects).size !== projects.length) {
    dialogError.value = "admin.invalidServiceProjects";
    return null;
  }
  if (permissions.value.some(permission => !permissionOptions.value.some(option => option.value === permission)) || new Set(permissions.value).size !== permissions.value.length) {
    dialogError.value = "admin.invalidServicePermissions";
    return null;
  }
  return { projects, permissions: [...permissions.value] };
}
async function updateAccount(): Promise<void> {
  if (saving.value || !editing.value) return;
  if (!Number.isInteger(ttl.value) || ttl.value < 300 || ttl.value > 86400) { dialogError.value = "admin.invalidTokenTTL"; return; }
  const metadata = editing.value.metadata;
  if (!metadata?.name || !metadata.uid || !metadata.resourceVersion) { dialogError.value = "admin.machineGrantsConflict"; return; }
  const grants = editing.value.spec?.type === "service" ? serviceGrants() : {};
  if (!grants) return;
  saving.value = true;
  dialogError.value = "";
  try {
    await request<MachineAccount>(`/apis/iam.ebs/v1/machineaccounts/${encodeURIComponent(metadata.name)}`, {
      method: "PATCH", headers: { "Content-Type": "application/merge-patch+json" },
      body: JSON.stringify({ metadata: { uid: metadata.uid, resourceVersion: metadata.resourceVersion }, spec: { tokenTTLSeconds: ttl.value, ...grants } }),
    });
    success.value = t("admin.machineGrantsSaved", { name: metadata.name });
    createOpen.value = false;
    editing.value = null;
    reload();
  } catch (error) {
    dialogError.value = error instanceof ApiError && (error.status === 409 || error.status === 404)
      ? "admin.machineGrantsConflict" : errorTranslationKey(error, "admin.saveMachineGrantsFailed");
  } finally { saving.value = false; }
}
async function createAccount(): Promise<void> {
  if (saving.value) return;
  if (accountType.value !== "runner" && accountType.value !== "service") { dialogError.value = "admin.invalidMachineType"; return; }
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(name.value) || !Number.isInteger(ttl.value) || ttl.value < 300 || ttl.value > 86400) { dialogError.value = "admin.invalidMachine"; return; }
  const grants = accountType.value === "service" ? serviceGrants() : {};
  if (!grants) return;
  saving.value = true;
  dialogError.value = "";
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const generatedSecret = btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  try {
    await request<{ name: string }>("/auth/machineaccounts", {
      method: "POST",
      body: JSON.stringify({
        name: name.value, type: accountType.value, clientSecret: generatedSecret, tokenTTLSeconds: ttl.value,
        ...grants,
      }),
    });
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

<style scoped>
.service-permissions { display: grid; gap: 12px; margin: 4px 0 12px; }
.service-permissions label { display: flex; align-items: flex-start; gap: 8px; font-size: 14px; cursor: pointer; }
.service-permissions input { margin-top: 3px; accent-color: var(--blue); }
.service-permissions span { display: flex; flex-wrap: wrap; gap: 4px 8px; }
.service-permissions code { color: var(--muted); font-size: 13px; }
</style>
