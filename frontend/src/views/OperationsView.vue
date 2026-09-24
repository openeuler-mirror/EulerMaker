<template>
  <section class="page-heading"><div><h1>{{ t("operations.title") }}</h1><p>{{ t("operations.hint") }}</p></div></section>
  <div class="operations-layout">
    <nav class="operations-menu" role="tablist" :aria-label="t('operations.menuLabel')">
      <button v-for="item in operationSections" :id="`operations-tab-${item.id}`" :key="item.id" type="button" role="tab" :aria-selected="activeSection === item.id" :aria-controls="`operations-panel-${item.id}`" :class="{ active: activeSection === item.id }" @click="selectSection(item.id)">{{ item.label }}</button>
    </nav>
    <main class="operations-content">
      <section v-if="activeSection === 'build'" id="operations-panel-build" role="tabpanel" aria-labelledby="operations-tab-build"><BuildTargetConfigEditor /></section>
      <section v-else-if="activeSection === 'scripts'" id="operations-panel-scripts" role="tabpanel" aria-labelledby="operations-tab-scripts"><ScriptManager /></section>

      <section v-else-if="activeSection === 'resources'" id="operations-panel-resources" class="content-panel" role="tabpanel" aria-labelledby="operations-tab-resources">
        <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
    <div class="section-heading"><h2>{{ t("operations.resources") }}</h2></div>
    <p class="form-hint">{{ t("operations.resourcesHint") }}</p>
    <div class="operations-project-form"><button class="secondary-button" type="button" :disabled="resourcesLoading" @click="loadResources">{{ t("common.refresh") }}</button></div>
    <div v-if="resourcesError" class="inline-error" role="alert"><WarningFilled />{{ t(resourcesError) }}</div>
    <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('operations.loadingResources')"><span v-for="item in 3" :key="item"></span></div><template v-else><EmptyState v-if="!resources.length" :title="t('operations.noResources')" :description="t('operations.noResourcesHint')" /><div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("operations.resourceName") }}</th><th>{{ t("operations.defaultCPU") }}</th><th>{{ t("operations.defaultMemory") }}</th><th>{{ t("operations.packageCount") }}</th><th>{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="resource in resources" :key="resource.config.metadata?.name"><td><strong>{{ resource.config.metadata?.name }}</strong></td><td>{{ resource.content.default?.requests?.cpu || t("common.emptyValue") }}</td><td>{{ resource.content.default?.requests?.memory || t("common.emptyValue") }}</td><td>{{ Object.keys(resource.content.packages || {}).length }}</td><td class="admin-row-actions"><button class="text-button" type="button" @click="openEdit(resource)">{{ t("common.edit") }}</button></td></tr></tbody></table></div></template>
        <section v-if="editorOpen" class="operations-resource-editor" aria-labelledby="resource-editor-title">
          <div class="section-heading"><h3 id="resource-editor-title">{{ t('operations.editResource') }}</h3></div>
          <form class="project-form" @submit.prevent="saveResource"><BuildResourceContentEditor ref="contentEditor" :source="contentSource" :disabled="saving" /><div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div><div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("common.saving") : t("common.save") }}</button></div></form>
        </section>
      </section>

      <section v-else id="operations-panel-runners" class="content-panel" role="tabpanel" aria-labelledby="operations-tab-runners"><div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div><div class="section-heading"><h2>{{ t("operations.runners") }}</h2><button class="icon-button runner-refresh-button" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="runnersLoading" @click="loadRunners"><Refresh /></button></div><form class="operations-project-form" @submit.prevent="runnerQuery = runnerSearch.trim()">
        <label class="field operations-project-field"><span>{{ t('operations.searchRunners') }}</span><input v-model="runnerSearch" type="search" :placeholder="t('operations.runnerSearchPlaceholder')" /></label>
        <button class="secondary-button" type="submit" :disabled="runnersLoading">{{ t('operations.search') }}</button>
      </form><div v-if="runnersLoading" class="skeleton-list" :aria-label="t('operations.loadingRunners')"><span v-for="item in 4" :key="item"></span></div><div v-else-if="runnersError" class="inline-error" role="alert"><WarningFilled />{{ t(runnersError) }}<button type="button" @click="loadRunners">{{ t("common.reload") }}</button></div><EmptyState v-else-if="!runners.length" :title="t('operations.noRunners')" :description="t('operations.noRunnersHint')" /><EmptyState v-else-if="!filteredRunners.length" :title="t('operations.noMatchingRunners')" :description="t('operations.noMatchingRunnersHint')" /><div v-else class="project-table-wrap"><table class="project-table admin-table"><thead><tr><th>{{ t("operations.runnerName") }}</th><th>{{ t("operations.phase") }}</th><th>{{ t("operations.type") }}</th><th>{{ t("operations.arch") }}</th><th>{{ t("operations.schedulable") }}</th><th>{{ t("operations.heartbeat") }}</th><th v-if="canManageRunners">{{ t("admin.actions") }}</th></tr></thead><tbody><tr v-for="runner in filteredRunners" :key="runner.metadata?.name"><td><strong>{{ runner.metadata?.name }}</strong><small class="admin-cell-subtitle">{{ runner.spec?.instanceId }}</small></td><td><StatusBadge :value="runner.status?.phase" /></td><td>{{ runner.spec?.type || t("common.emptyValue") }}</td><td>{{ runner.spec?.arch || t("common.emptyValue") }}</td><td>{{ t(runner.spec?.unschedulable || runner.status?.phase !== 'Online' ? 'common.no' : 'common.yes') }}</td><td>{{ formatDate(runner.status?.heartbeat) }}</td><td v-if="canManageRunners" class="admin-row-actions"><button class="text-button danger-link" type="button" :disabled="evictionSaving || !runner.metadata?.name" @click="openEviction(runner)">{{ t(runner.status?.phase === "Evicted" ? "operations.cancelEviction" : "operations.evict") }}</button></td></tr></tbody></table></div></section>
    </main>
  </div>

  <ModalDialog v-if="evicting" title-id="evict-runner-title" :title="t(cancelEviction ? 'operations.cancelEvictionTitle' : 'operations.evictRunner', { name: evicting.metadata?.name })" :close-label="t('common.close')" @close="closeEviction">
    <form class="project-form" @submit.prevent="evictRunner">
      <p class="form-hint">{{ t(cancelEviction ? "operations.cancelEvictionHint" : "operations.evictHint") }}</p>
      <div v-if="evictionError" class="form-error" role="alert"><WarningFilled />{{ t(evictionError) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="evictionSaving" @click="closeEviction">{{ t("common.cancel") }}</button>
        <button class="primary-button danger-button" type="submit" :disabled="evictionSaving">{{ t(evictionSaving ? "common.saving" : cancelEviction ? "operations.cancelEviction" : "operations.evict") }}</button>
      </div>
    </form>
  </ModalDialog>
</template>

<script setup lang="ts">
import { CircleCheckFilled, Refresh, WarningFilled } from "@element-plus/icons-vue";
import { computed, ref } from "vue";
import { parse, stringify } from "yaml";
import { useI18n } from "vue-i18n";
import { errorTranslationKey, list, request } from "@/api";
import { useSessionStore } from "@/stores/session";
import StatusBadge from "@/components/StatusBadge.vue";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import BuildResourceContentEditor from "@/components/BuildResourceContentEditor.vue";
import BuildTargetConfigEditor from "@/components/BuildTargetConfigEditor.vue";
import ScriptManager from "@/components/ScriptManager.vue";
import type { BuildResourceContent, Config, Runner } from "@/types";

type OperationsSection = "build" | "resources" | "scripts" | "runners";
type ResourceConfigView = { config: Config; content: BuildResourceContent };

const { t } = useI18n();
const session = useSessionStore();
const activeSection = ref<OperationsSection>("build");
const operationSections = computed<Array<{ id: OperationsSection; label: string }>>(() => [
  { id: "build", label: t("operations.buildConfiguration") },
  { id: "resources", label: t("operations.resourceConfiguration") },
  { id: "scripts", label: t("scripts.title") },
  { id: "runners", label: t("operations.runnerManagement") },
]);
const canManageRunners = computed(() => session.role === "admin" || session.role === "ops");
const evicting = ref<Runner | null>(null);
const evictionSaving = ref(false);
const cancelEviction = ref(false);
const evictionError = ref("");
const runners = ref<Runner[]>([]);
const runnerSearch = ref("");
const runnerQuery = ref("");
const filteredRunners = computed(() => {
  const query = runnerQuery.value.toLowerCase();
  return runners.value.filter(runner => (runner.metadata?.name || "").toLowerCase().includes(query));
});
const runnersLoading = ref(false);
const runnersError = ref("");
const resources = ref<ResourceConfigView[]>([]);
const resourcesLoading = ref(false);
const resourcesError = ref("");
const success = ref("");
const editorOpen = ref(false);
const editing = ref<ResourceConfigView | null>(null);
const contentSource = ref("");
const contentEditor = ref<InstanceType<typeof BuildResourceContentEditor> | null>(null);
const dialogError = ref("");
const saving = ref(false);

function selectSection(section: OperationsSection): void {
  activeSection.value = section;
  success.value = "";
  if (section === "resources" && !resources.value.length && !resourcesLoading.value) void loadResources();
  if (section === "runners" && !runners.value.length && !runnersLoading.value) void loadRunners();
}
async function loadRunners(): Promise<void> {
  runnersLoading.value = true;
  runnersError.value = "";
  try {
    runners.value = await loadAll<Runner>("/apis/ebs/v1/runners");
  } catch (error) { runnersError.value = errorTranslationKey(error, "operations.loadRunnersFailed"); }
  finally { runnersLoading.value = false; }
}
function openEviction(runner: Runner): void {
  if (!canManageRunners.value || evictionSaving.value) return;
  evicting.value = runner;
  cancelEviction.value = runner.status?.phase === "Evicted";
  evictionError.value = "";
  success.value = "";
}
function closeEviction(): void { if (!evictionSaving.value) evicting.value = null; }
async function evictRunner(): Promise<void> {
  const selected = evicting.value;
  const name = selected?.metadata?.name;
  if (!canManageRunners.value || !name || evictionSaving.value) return;
  evictionSaving.value = true;
  evictionError.value = "";
  try {
    const path = `/apis/ebs/v1/runners/${encodeURIComponent(name)}`;
    const current = await request<Runner>(path);
    if (!selected.metadata?.uid || current.metadata?.uid !== selected.metadata.uid || !current.metadata?.resourceVersion) {
      evictionError.value = "errors.conflict";
      return;
    }
    const shouldUpdate = cancelEviction.value ? current.status?.phase === "Evicted" : current.status?.phase !== "Evicted";
    if (shouldUpdate) {
      await request<Runner>(path + "/status", {
        method: "PATCH",
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({ metadata: { resourceVersion: current.metadata.resourceVersion }, status: { phase: cancelEviction.value ? "Offline" : "Evicted" } }),
      });
    }
    success.value = t(cancelEviction.value ? "operations.evictionCancelled" : "operations.runnerEvicted", { name });
    evicting.value = null;
    await loadRunners();
  } catch (error) { evictionError.value = errorTranslationKey(error, cancelEviction.value ? "operations.cancelEvictionFailed" : "operations.evictFailed"); }
  finally { evictionSaving.value = false; }
}
async function loadResources(): Promise<void> {
  resourcesLoading.value = true;
  resourcesError.value = "";
  try {
    const config = await request<Config>(resourcePath("build-resource"));
    resources.value = [{ config, content: parse(config.spec.content, { uniqueKeys: true }) as BuildResourceContent }];
  } catch (error) { resourcesError.value = errorTranslationKey(error, "operations.loadResourcesFailed"); }
  finally { resourcesLoading.value = false; }
}
async function openEdit(resource: ResourceConfigView): Promise<void> {
  if (!resource.config.metadata?.name) return;
  dialogError.value = "";
  try {
    const config = await request<Config>(resourcePath(resource.config.metadata.name));
    editing.value = { config, content: parse(config.spec.content, { uniqueKeys: true }) as BuildResourceContent };
    contentSource.value = JSON.stringify(editing.value.content, null, 2);
    editorOpen.value = true;
  } catch (error) { resourcesError.value = errorTranslationKey(error, "operations.loadResourcesFailed"); }
}
function closeEditor(): void { if (!saving.value) { editorOpen.value = false; editing.value = null; } }
function resourcePath(name: string): string { return `/apis/ebs/v1/configs/${encodeURIComponent(name)}`; }
async function refreshResources(): Promise<void> { await loadResources(); }
async function saveResource(): Promise<void> {
  if (saving.value) return;
  let content: BuildResourceContent;
  try {
    if (!contentEditor.value) return;
    content = contentEditor.value.getContent() as BuildResourceContent;
  } catch { return; }
  const current = editing.value;
  const name = current?.config.metadata?.name;
  if (!current || !name) return;
  saving.value = true;
  dialogError.value = "";
  const body: Config = { ...current.config, spec: { ...current.config.spec, content: stringify(content) } };
  try {
    await request<Config>(resourcePath(name), { method: "PUT", body: JSON.stringify(body) });
    success.value = t("operations.resourceSaved", { name });
    editorOpen.value = false;
    editing.value = null;
    await refreshResources();
  } catch (error) { dialogError.value = errorTranslationKey(error, "operations.saveResourceFailed"); }
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

<style scoped>
.runner-refresh-button { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.runner-refresh-button :deep(svg) { width: 16px; height: 16px; display: block; }
</style>
