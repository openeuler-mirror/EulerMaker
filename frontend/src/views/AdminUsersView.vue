<template>
  <section class="page-heading"><div><p>{{ t("admin.usersHint") }}</p></div><div class="page-actions"><button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="loading" @click="reload"><Refresh /></button></div></section>
  <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
  <section class="content-panel">
    <div class="list-toolbar"><label class="search-box"><Search /><input v-model.trim="search" type="search" :placeholder="t('admin.searchUsers')" /></label></div>
    <div v-if="loading" class="skeleton-list" :aria-label="t('admin.loadingUsers')"><span v-for="item in 5" :key="item"></span></div>
    <div v-else-if="loadError" class="inline-error" role="alert"><WarningFilled />{{ t(loadError) }}<button type="button" @click="loadUsers">{{ t("common.reload") }}</button></div>
    <EmptyState v-else-if="!filtered.length" :title="t('admin.noUsers')" :description="t('admin.noUsersHint')" />
    <div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("auth.username") }}</th><th>{{ t("admin.displayName") }}</th><th>{{ t("admin.role") }}</th><th>{{ t("admin.enabled") }}</th><th>{{ t("admin.email") }}</th><th>{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="user in filtered" :key="user.metadata?.name"><td><strong>{{ user.metadata?.name }}</strong></td><td>{{ user.spec?.displayName || t("common.emptyValue") }}</td><td>{{ roleLabel(user) }}</td><td><span :class="['boolean-status', { enabled: user.spec?.enabled !== false }]">{{ t(user.spec?.enabled === false ? 'admin.disabled' : 'admin.active') }}</span></td><td>{{ user.spec?.email || t("common.emptyValue") }}</td><td class="admin-row-actions"><button class="text-button" type="button" @click="openEdit(user)">{{ t("common.edit") }}</button><button class="text-button danger-link" type="button" @click="openDelete(user)">{{ t("common.remove") }}</button></td></tr></tbody></table></div>
    <div v-if="!loading && !loadError" class="table-footer"><span>{{ t("common.count", { count: filtered.length }) }}</span><div class="pagination-row"><button class="page-button arrow-button" type="button" :disabled="page === 1" :aria-label="t('common.previous')" @click="changePage(-1)"><ArrowLeft /></button><strong>{{ page }}</strong><button class="page-button arrow-button" type="button" :disabled="!nextToken" :aria-label="t('common.next')" @click="changePage(1)"><ArrowRight /></button></div></div>
  </section>

  <ModalDialog v-if="editing" title-id="edit-user-title" :title="t('admin.editUser', { name: editing.metadata?.name })" :close-label="t('common.close')" @close="closeDialog">
    <form class="project-form" @submit.prevent="saveUser">
      <div class="form-grid"><label class="field"><span>{{ t("admin.displayName") }}</span><input v-model.trim="draft.displayName" /></label><label class="field"><span>{{ t("admin.email") }}</span><input v-model.trim="draft.email" type="email" /></label></div>
      <div class="form-grid"><div class="field"><span>{{ t("admin.role") }}</span><AppSelect v-model="draft.scope" :options="[{ value: 'ebs:user', label: t('admin.regularUser') }, { value: 'ebs:ops', label: t('admin.opsUser') }]" :label="t('admin.role')" /></div><div class="field"><span>{{ t("admin.enabled") }}</span><AppSelect :model-value="String(draft.enabled)" :options="[{ value: 'true', label: t('admin.active') }, { value: 'false', label: t('admin.disabled') }]" :label="t('admin.enabled')" @update:model-value="draft.enabled = $event === 'true'" /></div></div>
      <div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeDialog">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("common.saving") : t("common.save") }}</button></div>
    </form>
  </ModalDialog>

  <ModalDialog v-if="deleting" title-id="delete-user-title" :title="t('admin.deleteUser', { name: deleting.metadata?.name })" :close-label="t('common.close')" @close="closeDialog">
    <form class="project-form" @submit.prevent="deleteUser"><p class="form-hint">{{ t("admin.typeNameHint", { name: deleting.metadata?.name }) }}</p><label class="field"><span>{{ t("auth.username") }}</span><input v-model.trim="confirmName" autocomplete="off" required /></label><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeDialog">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="submit" :disabled="saving || confirmName !== deleting.metadata?.name">{{ t("common.remove") }}</button></div></form>
  </ModalDialog>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, CircleCheckFilled, Refresh, Search, WarningFilled } from "@element-plus/icons-vue";
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import AppSelect from "@/components/AppSelect.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import type { ManagedUser } from "@/types";

const { t } = useI18n();
const users = ref<ManagedUser[]>([]);
const loading = ref(true);
const loadError = ref("");
const dialogError = ref("");
const success = ref("");
const search = ref("");
const page = ref(1);
const tokens = ref([""]);
const nextToken = ref("");
const editing = ref<ManagedUser | null>(null);
const deleting = ref<ManagedUser | null>(null);
const confirmName = ref("");
const saving = ref(false);
const draft = reactive({ displayName: "", email: "", scope: "ebs:user", enabled: true });
const filtered = computed(() => users.value.filter((user) => [user.metadata?.name, user.spec?.displayName, user.spec?.email].some((value) => value?.toLowerCase().includes(search.value.toLowerCase()))));

onMounted(() => { void loadUsers(); });

function roleLabel(user: ManagedUser): string { return t(user.spec?.scopes?.includes("ebs:ops") ? "admin.opsUser" : "admin.regularUser"); }
async function loadUsers(): Promise<void> {
  loading.value = true;
  loadError.value = "";
  try {
    const query = new URLSearchParams({ limit: "50" });
    const token = tokens.value[page.value - 1];
    if (token) query.set("continue", token);
    const result = await list<ManagedUser>(`/apis/iam.ebs/v1/users?${query}`);
    users.value = result.items;
    nextToken.value = result.next;
    if (result.next) tokens.value[page.value] = result.next;
  } catch (error) { loadError.value = errorTranslationKey(error, "admin.loadUsersFailed"); }
  finally { loading.value = false; }
}
function reload(): void { page.value = 1; tokens.value = [""]; void loadUsers(); }
function changePage(direction: number): void { page.value += direction; void loadUsers(); }
async function openEdit(user: ManagedUser): Promise<void> {
  dialogError.value = "";
  success.value = "";
  try {
    const current = await request<ManagedUser>(`/apis/iam.ebs/v1/users/${encodeURIComponent(user.metadata?.name || "")}`);
    editing.value = current;
    draft.displayName = current.spec?.displayName || "";
    draft.email = current.spec?.email || "";
    draft.scope = current.spec?.scopes?.includes("ebs:ops") ? "ebs:ops" : "ebs:user";
    draft.enabled = current.spec?.enabled !== false;
  } catch (error) { loadError.value = errorTranslationKey(error, "admin.loadUsersFailed"); }
}
function openDelete(user: ManagedUser): void { deleting.value = user; confirmName.value = ""; dialogError.value = ""; success.value = ""; }
function closeDialog(): void { if (saving.value) return; editing.value = null; deleting.value = null; dialogError.value = ""; }
async function saveUser(): Promise<void> {
  const user = editing.value;
  if (!user?.metadata?.name || !user.metadata.resourceVersion || saving.value) return;
  saving.value = true;
  dialogError.value = "";
  try {
    await request<ManagedUser>(`/apis/iam.ebs/v1/users/${encodeURIComponent(user.metadata.name)}`, { method: "PATCH", headers: { "Content-Type": "application/merge-patch+json" }, body: JSON.stringify({ metadata: { resourceVersion: user.metadata.resourceVersion }, spec: { displayName: draft.displayName, email: draft.email, scopes: [draft.scope], enabled: draft.enabled } }) });
    success.value = t("admin.userSaved", { name: user.metadata.name });
    editing.value = null;
    await loadUsers();
  } catch (error) { dialogError.value = errorTranslationKey(error, "admin.saveUserFailed"); }
  finally { saving.value = false; }
}
async function deleteUser(): Promise<void> {
  const name = deleting.value?.metadata?.name;
  if (!name || confirmName.value !== name || saving.value) return;
  saving.value = true;
  dialogError.value = "";
  try {
    await request<unknown>(`/apis/iam.ebs/v1/users/${encodeURIComponent(name)}`, { method: "DELETE" });
    success.value = t("admin.userDeleted", { name });
    deleting.value = null;
    await loadUsers();
  } catch (error) { dialogError.value = errorTranslationKey(error, "admin.deleteUserFailed"); }
  finally { saving.value = false; }
}
</script>
