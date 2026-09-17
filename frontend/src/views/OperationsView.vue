<template>
  <section class="page-heading"><div><h1>{{ t("operations.title") }}</h1><p>{{ t("operations.hint") }}</p></div><div class="page-actions"><button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="runnersLoading" @click="loadRunners"><Refresh /></button></div></section>
  <BuildConfEditor />
  <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>

  <section class="content-panel operations-section">
    <div class="section-heading"><h2>{{ t("operations.resources") }}</h2></div>
    <p class="form-hint">{{ t("operations.resourcesHint") }}</p>
    <form class="operations-project-form" @submit.prevent="loadResources"><label class="field required-field operations-project-field"><span>{{ t("operations.projectName") }}</span><input v-model.trim="projectName" required maxlength="63" autocomplete="off" :placeholder="t('operations.projectPlaceholder')" /></label><button class="secondary-button" type="submit" :disabled="resourcesLoading">{{ t("operations.loadResources") }}</button><button v-if="loadedProject" class="primary-button" type="button" @click="openCreate">{{ t("operations.createResource") }}</button></form>
    <div v-if="resourcesError" class="inline-error" role="alert"><WarningFilled />{{ t(resourcesError) }}</div>
    <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('operations.loadingResources')"><span v-for="item in 3" :key="item"></span></div><template v-else-if="loadedProject"><EmptyState v-if="!resources.length" :title="t('operations.noResources')" :description="t('operations.noResourcesHint')" /><div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("operations.resourceName") }}</th><th>{{ t("operations.defaultCPU") }}</th><th>{{ t("operations.defaultMemory") }}</th><th>{{ t("operations.packageCount") }}</th><th>{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="resource in resources" :key="resource.metadata?.name"><td><strong>{{ resource.metadata?.name }}</strong></td><td>{{ resource.spec?.default?.requests?.cpu || t("common.emptyValue") }}</td><td>{{ resource.spec?.default?.requests?.memory || t("common.emptyValue") }}</td><td>{{ Object.keys(resource.spec?.packages || {}).length }}</td><td class="admin-row-actions"><button class="text-button" type="button" @click="openEdit(resource)">{{ t("common.edit") }}</button><button class="text-button danger-link" type="button" :disabled="isProtectedDefaultResource(resource)" :title="isProtectedDefaultResource(resource) ? t('operations.defaultResourceProtected') : undefined" @click="openDelete(resource)">{{ t("common.remove") }}</button></td></tr></tbody></table></div></template>
  </section>

  <section class="content-panel operations-section"><div class="section-heading"><h2>{{ t("operations.runners") }}</h2></div><div v-if="runnersLoading" class="skeleton-list" :aria-label="t('operations.loadingRunners')"><span v-for="item in 4" :key="item"></span></div><div v-else-if="runnersError" class="inline-error" role="alert"><WarningFilled />{{ t(runnersError) }}<button type="button" @click="loadRunners">{{ t("common.reload") }}</button></div><EmptyState v-else-if="!runners.length" :title="t('operations.noRunners')" :description="t('operations.noRunnersHint')" /><div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("operations.runnerName") }}</th><th>{{ t("operations.phase") }}</th><th>{{ t("operations.type") }}</th><th>{{ t("operations.arch") }}</th><th>{{ t("operations.schedulable") }}</th><th>{{ t("operations.heartbeat") }}</th></tr></thead><tbody><tr v-for="runner in runners" :key="runner.metadata?.name"><td><strong>{{ runner.metadata?.name }}</strong><small class="admin-cell-subtitle">{{ runner.spec?.instanceId }}</small></td><td>{{ runner.status?.phase || t("common.unknown") }}</td><td>{{ runner.spec?.type || t("common.emptyValue") }}</td><td>{{ runner.spec?.arch || t("common.emptyValue") }}</td><td>{{ t(runner.spec?.unschedulable ? 'common.no' : 'common.yes') }}</td><td>{{ formatDate(runner.status?.heartbeat) }}</td></tr></tbody></table></div></section>

  <ModalDialog v-if="editorOpen" title-id="resource-editor-title" :title="t(editing ? 'operations.editResource' : 'operations.createResource')" :close-label="t('common.close')" @close="closeEditor"><form class="project-form" @submit.prevent="saveResource"><label class="field required-field"><span>{{ t("operations.resourceName") }}</span><input v-model.trim="resourceName" required maxlength="63" :disabled="Boolean(editing)" /></label><BuildResourceSpecEditor ref="specEditor" :source="specSource" :disabled="saving" /><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("common.saving") : t("common.save") }}</button></div></form></ModalDialog>
  <ModalDialog v-if="deleting" title-id="delete-resource-title" :title="t('operations.deleteResource', { name: deleting.metadata?.name })" :close-label="t('common.close')" @close="closeDelete"><form class="project-form" @submit.prevent="deleteResource"><p class="form-hint">{{ t("admin.typeNameHint", { name: deleting.metadata?.name }) }}</p><label class="field"><span>{{ t("operations.resourceName") }}</span><input v-model.trim="confirmName" required autocomplete="off" /></label><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeDelete">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="submit" :disabled="saving || confirmName !== deleting.metadata?.name">{{ t("common.remove") }}</button></div></form></ModalDialog>
</template>

<script setup lang="ts">
import { CircleCheckFilled, Refresh, WarningFilled } from "@element-plus/icons-vue";
import { onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import BuildResourceSpecEditor from "@/components/BuildResourceSpecEditor.vue";
import BuildConfEditor from "@/components/BuildConfEditor.vue";
import type { BuildResource, Runner } from "@/types";

const { t } = useI18n();
const runners = ref<Runner[]>([]);
const runnersLoading = ref(true);
const runnersError = ref("");
const projectName = ref("default");
const loadedProject = ref("");
const resources = ref<BuildResource[]>([]);
const resourcesLoading = ref(false);
const resourcesError = ref("");
const success = ref("");
const editorOpen = ref(false);
const editing = ref<BuildResource | null>(null);
const resourceName = ref("");
const specSource = ref("");
const specEditor = ref<InstanceType<typeof BuildResourceSpecEditor> | null>(null);
const deleting = ref<BuildResource | null>(null);
const confirmName = ref("");
const dialogError = ref("");
const saving = ref(false);

onMounted(() => { void loadRunners(); void loadResources(); });
async function loadRunners(): Promise<void> {
  runnersLoading.value = true;
  runnersError.value = "";
  try {
    runners.value = await loadAll<Runner>("/apis/ebs/v1/runners");
  } catch (error) { runnersError.value = errorTranslationKey(error, "operations.loadRunnersFailed"); }
  finally { runnersLoading.value = false; }
}
async function loadResources(): Promise<void> {
  const project = projectName.value;
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(project)) { resourcesError.value = "operations.invalidProject"; return; }
  resourcesLoading.value = true;
  resourcesError.value = "";
  loadedProject.value = "";
  try {
    resources.value = await loadAll<BuildResource>(`/apis/ebs/v1/projects/${encodeURIComponent(project)}/buildresources`);
    loadedProject.value = project;
  } catch (error) { resourcesError.value = errorTranslationKey(error, "operations.loadResourcesFailed"); }
  finally { resourcesLoading.value = false; }
}
function openCreate(): void {
  editing.value = null;
  resourceName.value = loadedProject.value;
  specSource.value = JSON.stringify({ default: { requests: { cpu: "1", memory: "2Gi" } }, packages: { example: { default: { requests: { cpu: "1", memory: "2Gi" } } } } }, null, 2);
  dialogError.value = "";
  editorOpen.value = true;
}
async function openEdit(resource: BuildResource): Promise<void> {
  if (!resource.metadata?.name) return;
  dialogError.value = "";
  try {
    editing.value = await request<BuildResource>(resourcePath(resource.metadata.name));
    resourceName.value = resource.metadata.name;
    specSource.value = JSON.stringify(editing.value.spec || {}, null, 2);
    editorOpen.value = true;
  } catch (error) { resourcesError.value = errorTranslationKey(error, "operations.loadResourcesFailed"); }
}
function closeEditor(): void { if (!saving.value) { editorOpen.value = false; editing.value = null; } }
function isProtectedDefaultResource(resource: BuildResource): boolean { return loadedProject.value === "default" && resource.metadata?.name === "default"; }
function openDelete(resource: BuildResource): void { if (isProtectedDefaultResource(resource)) return; deleting.value = resource; confirmName.value = ""; dialogError.value = ""; }
function closeDelete(): void { if (!saving.value) deleting.value = null; }
function resourcePath(name: string): string { return `/apis/ebs/v1/projects/${encodeURIComponent(loadedProject.value)}/buildresources/${encodeURIComponent(name)}`; }
async function refreshResources(): Promise<void> { projectName.value = loadedProject.value; await loadResources(); }
async function saveResource(): Promise<void> {
  if (saving.value) return;
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(resourceName.value)) { dialogError.value = "operations.invalidResource"; return; }
  let spec: BuildResource["spec"];
  try {
    if (!specEditor.value) return;
    spec = specEditor.value.getSpec() as BuildResource["spec"];
  } catch { return; }
  saving.value = true;
  dialogError.value = "";
  const current = editing.value;
  const body: BuildResource = current
    ? { ...current, spec }
    : { apiVersion: "ebs/v1", kind: "BuildResource", metadata: { name: resourceName.value, namespace: loadedProject.value }, spec };
  try {
    await request<BuildResource>(current ? resourcePath(resourceName.value) : `/apis/ebs/v1/projects/${encodeURIComponent(loadedProject.value)}/buildresources`, { method: current ? "PUT" : "POST", body: JSON.stringify(body) });
    success.value = t(current ? "operations.resourceSaved" : "operations.resourceCreated", { name: resourceName.value });
    editorOpen.value = false;
    editing.value = null;
    await refreshResources();
  } catch (error) { dialogError.value = errorTranslationKey(error, "operations.saveResourceFailed"); }
  finally { saving.value = false; }
}
async function deleteResource(): Promise<void> {
  const name = deleting.value?.metadata?.name;
  if (!name || (loadedProject.value === "default" && name === "default") || confirmName.value !== name || saving.value) return;
  saving.value = true;
  dialogError.value = "";
  try {
    await request<unknown>(resourcePath(name), { method: "DELETE" });
    success.value = t("operations.resourceDeleted", { name });
    deleting.value = null;
    await refreshResources();
  } catch (error) { dialogError.value = errorTranslationKey(error, "operations.deleteResourceFailed"); }
  finally { saving.value = false; }
}
function formatDate(value?: string): string {
  if (!value) return t("common.emptyValue");
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return t("common.emptyValue");
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
async function loadAll<T>(path: string): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let token = "";
  do {
    const query = new URLSearchParams({ limit: "100" });
    if (token) query.set("continue", token);
    const result = await list<T>(`${path}?${query}`);
    items.push(...result.items);
    token = result.next;
    if (token && seen.has(token)) throw new Error("repeated continuation token");
    seen.add(token);
  } while (token);
  return items;
}
</script>
