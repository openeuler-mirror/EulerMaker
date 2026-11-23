<template>
  <section class="page-heading"><div><h1>{{ t("operations.title") }}</h1><p>{{ t("operations.hint") }}</p></div></section>
  <div class="operations-layout">
    <nav class="operations-menu" role="tablist" :aria-label="t('operations.menuLabel')">
      <button v-for="item in operationSections" :id="`operations-tab-${item.id}`" :key="item.id" type="button" role="tab" :aria-selected="activeSection === item.id" :aria-controls="`operations-panel-${item.id}`" :class="{ active: activeSection === item.id }" @click="selectSection(item.id)">{{ item.label }}</button>
    </nav>
    <main class="operations-content">
      <section v-if="activeSection === 'build'" id="operations-panel-build" role="tabpanel" aria-labelledby="operations-tab-build"><BuildTargetConfigEditor /></section>
      <section v-else-if="activeSection === 'services'" id="operations-panel-services" role="tabpanel" aria-labelledby="operations-tab-services"><ServiceConfigEditor /></section>
      <section v-else-if="activeSection === 'scripts'" id="operations-panel-scripts" role="tabpanel" aria-labelledby="operations-tab-scripts"><ScriptManager /></section>

      <section v-else-if="activeSection === 'resources'" id="operations-panel-resources" class="content-panel" role="tabpanel" aria-labelledby="operations-tab-resources">
        <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
        <div class="section-heading"><h2>{{ t("operations.resources") }}</h2></div>
        <p class="form-hint">{{ t("operations.resourcesHint") }}</p>
        <div class="operations-project-form"><button class="secondary-button" type="button" :disabled="resourcesLoading || saving" @click="loadResources">{{ t("common.refresh") }}</button></div>
        <div v-if="resourcesError" class="inline-error" role="alert"><WarningFilled />{{ t(resourcesError) }}</div>
        <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('operations.loadingResources')"><span v-for="item in 3" :key="item"></span></div>
        <section v-else-if="editing" class="operations-resource-editor" :aria-label="t('operations.resources')">
          <form class="project-form" @submit.prevent="saveResource">
            <BuildResourceContentEditor :key="editorRevision" ref="contentEditor" :source="contentSource" :disabled="saving" />
            <div v-if="dialogError" class="form-error" role="alert"><WarningFilled />{{ t(dialogError) }}</div>
            <div class="modal-actions">
              <button class="secondary-button" type="button" :disabled="saving" @click="resetEditor">{{ t("common.cancel") }}</button>
              <button class="primary-button" type="submit" :disabled="saving">{{ saving ? t("common.saving") : t("common.save") }}</button>
            </div>
          </form>
        </section>
      </section>

      <section v-else id="operations-panel-runners" class="content-panel" role="tabpanel" aria-labelledby="operations-tab-runners">
        <div v-if="success" class="success-banner" role="status"><CircleCheckFilled />{{ success }}</div>
        <div class="section-heading">
          <h2>{{ t("operations.runners") }}</h2>
          <button class="icon-button runner-refresh-button" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="runnersLoading" @click="loadRunners"><Refresh /></button>
        </div>
        <form class="operations-project-form" @submit.prevent="searchRunners">
          <div class="field operations-project-field runner-search-field">
            <input v-model="runnerSearch" type="search" role="combobox" autocomplete="off" aria-autocomplete="list" aria-controls="runner-search-suggestions" :aria-expanded="suggestionsOpen && runnerSuggestions.length > 0" :aria-activedescendant="suggestionsOpen && activeSuggestion >= 0 ? `runner-suggestion-${activeSuggestion}` : undefined" :aria-label="t('operations.searchRunners')" :placeholder="t('operations.runnerSearchPlaceholder')" :title="t('operations.runnerSearchHint')" @focus="showSuggestions" @input="showSuggestions" @blur="suggestionsOpen = false" @keydown="onSearchKeydown" />
            <button class="runner-search-button" type="submit" :disabled="runnersLoading" :aria-label="t('operations.search')" :title="t('operations.search')"><Search aria-hidden="true" /></button>
            <ul v-if="suggestionsOpen && runnerSuggestions.length" id="runner-search-suggestions" ref="suggestionList" class="runner-search-suggestions" role="listbox" :aria-label="t('operations.searchRunners')">
              <li v-for="(suggestion, index) in runnerSuggestions" :id="`runner-suggestion-${index}`" :key="suggestion" role="option" :aria-selected="index === activeSuggestion" :class="{ active: index === activeSuggestion }" @mousedown.prevent @click="chooseSuggestion(index)">{{ suggestion }}</li>
            </ul>
          </div>
        </form>
        <div v-if="runnerSearchError" class="inline-error" role="alert">{{ t(runnerSearchError) }}</div>
        <div v-if="runnersLoading" class="skeleton-list" :aria-label="t('operations.loadingRunners')"><span v-for="item in 4" :key="item"></span></div>
        <div v-else-if="runnersError" class="inline-error" role="alert"><WarningFilled />{{ t(runnersError) }}<button type="button" @click="loadRunners">{{ t("common.reload") }}</button></div>
        <EmptyState v-else-if="!runners.length && !runnerLabelQuery && !runnerQuery" :title="t('operations.noRunners')" :description="t('operations.noRunnersHint')" />
        <EmptyState v-else-if="!filteredRunners.length" :title="t('operations.noMatchingRunners')" :description="t('operations.noMatchingRunnersHint')" />
        <div v-else class="project-table-wrap">
          <table class="project-table admin-table">
            <thead><tr>
              <th>{{ t("operations.runnerName") }}</th><th>{{ t("operations.phase") }}</th><th>{{ t("operations.runnerTypeArch") }}</th>
              <th>{{ t("operations.runnerResources") }}</th>
              <th>{{ t("operations.agentVersion") }}</th><th>{{ t("operations.schedulable") }}</th><th>{{ t("operations.activeJobs") }}</th><th>{{ t("operations.heartbeat") }}</th>
              <th v-if="canManageRunners">{{ t("admin.actions") }}</th>
            </tr></thead>
            <tbody><tr v-for="runner in filteredRunners" :key="runner.metadata?.name">
              <td><RouterLink v-if="runner.metadata?.name" class="project-name-link" :to="{ name: 'runner', params: { name: runner.metadata.name } }"><strong>{{ runner.metadata.name }}</strong></RouterLink><small class="admin-cell-subtitle">{{ runner.spec?.instanceID }}</small></td>
              <td><StatusBadge :value="runner.status?.phase" /></td>
              <td class="runner-type-arch">{{ runner.spec?.type || t("common.emptyValue") }} / {{ runner.spec?.arch || t("common.emptyValue") }}</td>
              <td class="runner-capacity">
                <span>{{ runner.status?.capacity?.cpu ? `${runner.status.capacity.cpu}U` : t("common.emptyValue") }}</span> / <span>{{ formatRunnerMemory(runner.status?.capacity?.memory) || t("common.emptyValue") }}</span>
              </td>
              <td><code class="runner-agent-version" :title="runner.status?.info?.agentVersion">{{ runner.status?.info?.agentVersion || t("common.emptyValue") }}</code></td>
              <td>{{ t(runner.spec?.unschedulable || runner.status?.phase !== 'Online' ? 'common.no' : 'common.yes') }}</td>
              <td>{{ runner.status?.activeJobs ?? 0 }}</td>
              <td>{{ formatDate(runner.status?.heartbeat) }}</td>
              <td v-if="canManageRunners" class="admin-row-actions">
                <button class="text-button" type="button" :disabled="labelsSaving || evictionSaving || removalSaving || !runner.metadata?.uid" @click="openRunnerLabels(runner)">{{ t('operations.editRunnerLabels') }}</button>
                <button class="text-button danger-link" type="button" :disabled="evictionSaving || removalSaving || !runner.metadata?.name" @click="openEviction(runner)">{{ t(runner.status?.phase === "Evicted" ? "operations.cancelEviction" : "operations.evict") }}</button>
                <button class="text-button danger-link" type="button" :disabled="evictionSaving || removalSaving || !runner.metadata?.name || !runner.metadata?.uid || !removableRunner(runner)" :title="removableRunner(runner) ? '' : t('operations.removeRunnerUnavailable')" @click="openRemoval(runner)">{{ t("operations.removeRunner") }}</button>
              </td>
            </tr></tbody>
          </table>
        </div>
      </section>
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
  <ModalDialog v-if="labelRunner" title-id="runner-labels-title" :title="t('operations.runnerLabelsTitle', { name: labelRunner.metadata?.name })" :close-label="t('common.close')" @close="closeRunnerLabels">
    <form class="project-form" @submit.prevent="saveRunnerLabels">
      <p class="form-hint">{{ t('operations.runnerLabelsHint') }}</p>
      <div v-for="(row, index) in labelRows" :key="index" class="runner-label-row">
        <label class="field"><span>{{ t('operations.labelKey') }}</span><input v-model="row.key" :disabled="labelsSaving || lockedLabelRows.has(row)" required /></label>
        <label class="field"><span>{{ t('operations.labelValue') }}</span><input v-model="row.value" :disabled="labelsSaving || lockedLabelRows.has(row)" maxlength="63" /></label>
        <button class="text-button danger-link" type="button" :disabled="labelsSaving || lockedLabelRows.has(row)" @click="labelRows.splice(index, 1)">{{ t('common.remove') }}</button>
      </div>
      <button class="secondary-button" type="button" :disabled="labelsSaving" @click="labelRows.push({ key: '', value: '' })">{{ t('operations.addRunnerLabel') }}</button>
      <div v-if="labelsError" class="form-error" role="alert"><WarningFilled />{{ t(labelsError) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="labelsSaving" @click="closeRunnerLabels">{{ t('common.cancel') }}</button>
        <button class="primary-button" type="submit" :disabled="labelsSaving">{{ t(labelsSaving ? 'common.saving' : 'common.save') }}</button>
      </div>
    </form>
  </ModalDialog>
  <ModalDialog v-if="removing" title-id="remove-runner-title" :title="t('operations.removeRunnerTitle', { name: removing.metadata?.name })" :close-label="t('common.close')" @close="closeRemoval">
    <form class="project-form" @submit.prevent="confirmRemoval">
      <p class="form-hint">{{ t('operations.removeRunnerHint') }}</p>
      <div v-if="removalError" class="form-error" role="alert"><WarningFilled />{{ t(removalError) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="removalSaving" @click="closeRemoval">{{ t('common.cancel') }}</button>
        <button class="primary-button danger-button" type="submit" :disabled="removalSaving || !removableRunner(removing)">{{ t('operations.removeRunner') }}</button>
      </div>
    </form>
  </ModalDialog>
</template>

<script setup lang="ts">
import { CircleCheckFilled, Refresh, Search, WarningFilled } from "@element-plus/icons-vue";
import { computed, nextTick, ref, watch } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";
import { parse, stringify } from "yaml";
import { useI18n } from "vue-i18n";
import { errorTranslationKey, list, request } from "@/api";
import { useSessionStore } from "@/stores/session";
import StatusBadge from "@/components/StatusBadge.vue";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import BuildResourceContentEditor from "@/components/BuildResourceContentEditor.vue";
import BuildTargetConfigEditor from "@/components/BuildTargetConfigEditor.vue";
import ServiceConfigEditor from "@/components/ServiceConfigEditor.vue";
import ScriptManager from "@/components/ScriptManager.vue";
import type { BuildResourceContent, Config, Runner } from "@/types";
import { formatRunnerMemory } from "@/utils/runnerResources";
import { compileRunnerLabelSelector, runnerSearchOptions, runnerSearchSuggestions } from "@/utils/runnerSearch";
import { isSystemRunnerLabel, runnerLabelPatch, runnerLabelRows, type RunnerLabelRow } from "@/utils/runnerLabels";

type OperationsSection = "build" | "services" | "resources" | "scripts" | "runners";
type ResourceConfigView = { config: Config; content: BuildResourceContent };

const { t } = useI18n();
const session = useSessionStore();
const route = useRoute();
const router = useRouter();
const operationSections = computed<Array<{ id: OperationsSection; label: string }>>(() => [
  { id: "build", label: t("operations.buildConfiguration") },
  { id: "services", label: t("serviceConfig.title") },
  { id: "resources", label: t("operations.resourceConfiguration") },
  { id: "scripts", label: t("scripts.title") },
  { id: "runners", label: t("operations.runnerManagement") },
]);
const activeSection = computed<OperationsSection>(() =>
  operationSections.value.find(item => item.id === route.query.section)?.id || "build",
);
const canManageRunners = computed(() => session.role === "admin" || session.role === "ops");
const labelRunner = ref<Runner | null>(null);
const labelRows = ref<RunnerLabelRow[]>([]);
const lockedLabelRows = ref(new Set<RunnerLabelRow>());
const labelsSaving = ref(false);
const labelsError = ref("");
const evicting = ref<Runner | null>(null);
const evictionSaving = ref(false);
const cancelEviction = ref(false);
const evictionError = ref("");
const removing = ref<Runner | null>(null);
const removalSaving = ref(false);
const removalError = ref("");
const runners = ref<Runner[]>([]);
const runnerSearch = ref("");
const searchOptions = computed(() => runnerSearchOptions(runners.value));
const runnerSuggestions = computed(() => runnerSearchSuggestions(runnerSearch.value, searchOptions.value));
const suggestionsOpen = ref(false);
const activeSuggestion = ref(-1);
const suggestionList = ref<HTMLElement | null>(null);
const runnerQuery = ref("");
const runnerLabelQuery = ref("");
const runnerSearchError = ref("");
const runnerLabelMatcher = computed(() => compileRunnerLabelSelector(runnerLabelQuery.value));
let runnerLoadID = 0;
const filteredRunners = computed(() => {
  const query = runnerQuery.value.toLowerCase();
  return runners.value.filter(runner => (runner.metadata?.name || "").toLowerCase().includes(query)
    && runnerLabelMatcher.value(runner.metadata?.labels || {}));
});
const runnersLoading = ref(false);
const runnersError = ref("");
const resourcesLoading = ref(false);
const resourcesError = ref("");
const success = ref("");
const editorRevision = ref(0);
const editing = ref<ResourceConfigView | null>(null);
const contentSource = ref("");
const contentEditor = ref<InstanceType<typeof BuildResourceContentEditor> | null>(null);
const dialogError = ref("");
const saving = ref(false);

function removableRunner(runner: Runner | null): boolean {
  return runner?.status?.phase === "Evicted" || runner?.status?.phase === "Offline";
}
function selectSection(section: OperationsSection): void {
  const query = { ...route.query };
  if (section === "build") delete query.section;
  else query.section = section;
  void router.replace({ query });
}
function loadSection(section: OperationsSection): void {
  success.value = "";
  if (section === "resources" && !editing.value && !resourcesLoading.value) void loadResources();
  if (section === "runners" && !runners.value.length && !runnersLoading.value) void loadRunners();
}
function searchRunners(): void {
  suggestionsOpen.value = false;
  const parts = runnerSearch.value.trim().split(/(?:^|\s+)(name|labels):\s*/g);
  const names = [parts[0].trim()].filter(Boolean);
  const labels: string[] = [];
  for (let index = 1; index < parts.length; index += 2) {
    const value = parts[index + 1].trim();
    if (value) (parts[index] === "labels" ? labels : names).push(value);
  }
  const selector = labels.join(",");
  try {
    compileRunnerLabelSelector(selector);
    runnerSearchError.value = "";
    runnerQuery.value = names.join(" ");
    runnerLabelQuery.value = selector;
  } catch { runnerSearchError.value = "operations.invalidRunnerSearch"; }
}
function showSuggestions(): void {
  suggestionsOpen.value = true;
  activeSuggestion.value = -1;
}
function chooseSuggestion(index: number): void {
  const value = runnerSuggestions.value[index];
  if (value === undefined) return;
  runnerSearch.value = value;
  suggestionsOpen.value = false;
  activeSuggestion.value = -1;
}
function onSearchKeydown(event: KeyboardEvent): void {
  if (event.isComposing) return;
  if (event.key === "Escape" && suggestionsOpen.value) {
    event.preventDefault();
    suggestionsOpen.value = false;
    return;
  }
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    const count = runnerSuggestions.value.length;
    if (!count) return;
    event.preventDefault();
    const current = suggestionsOpen.value ? activeSuggestion.value : -1;
    suggestionsOpen.value = true;
    activeSuggestion.value = current < 0 ? (event.key === "ArrowDown" ? 0 : count - 1)
      : (current + (event.key === "ArrowDown" ? 1 : -1) + count) % count;
    void nextTick(() => suggestionList.value?.children[activeSuggestion.value]?.scrollIntoView({ block: "nearest" }));
    return;
  }
  if (event.key === "Enter" || event.key === "Tab") {
    if (suggestionsOpen.value && activeSuggestion.value >= 0) {
      if (event.key === "Enter") event.preventDefault();
      chooseSuggestion(activeSuggestion.value);
    }
    suggestionsOpen.value = false;
  }
}
async function loadRunners(): Promise<void> {
  const loadID = ++runnerLoadID;
  runnersLoading.value = true;
  runnersError.value = "";
  try {
    const result = await loadAll<Runner>("/apis/ebs/v1/runners");
    if (loadID === runnerLoadID) runners.value = result;
  } catch (error) {
    if (loadID === runnerLoadID) runnersError.value = errorTranslationKey(error, "operations.loadRunnersFailed");
  } finally { if (loadID === runnerLoadID) runnersLoading.value = false; }
}
function openRunnerLabels(runner: Runner): void {
  if (!canManageRunners.value || labelsSaving.value || evicting.value || removing.value || !runner.metadata?.uid) return;
  labelRunner.value = { ...runner, metadata: { ...runner.metadata, labels: { ...runner.metadata.labels } } };
  labelRows.value = runnerLabelRows(runner.metadata.labels);
  lockedLabelRows.value = new Set(labelRows.value.filter(row => isSystemRunnerLabel(row.key)));
  labelsError.value = "";
  success.value = "";
}
function closeRunnerLabels(): void { if (!labelsSaving.value) labelRunner.value = null; }
async function saveRunnerLabels(): Promise<void> {
  const selected = labelRunner.value;
  const name = selected?.metadata?.name;
  if (!canManageRunners.value || !name || !selected.metadata?.uid || labelsSaving.value) return;
  labelsError.value = "";
  const original = selected.metadata.labels || {};
  const draft = runnerLabelPatch(original, labelRows.value, original);
  if (draft.error) { labelsError.value = draft.error; return; }
  labelsSaving.value = true;
  try {
    const path = `/apis/ebs/v1/runners/${encodeURIComponent(name)}`;
    const current = await request<Runner>(path);
    if (current.metadata?.uid !== selected.metadata.uid || !current.metadata?.resourceVersion) {
      labelsError.value = "errors.conflict";
      return;
    }
    const patch = runnerLabelPatch(original, labelRows.value, current.metadata.labels || {});
    if (patch.error) { labelsError.value = patch.error; return; }
    if (Object.keys(patch.labels).length) {
      await request<Runner>(path, {
        method: "PATCH",
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({ metadata: { uid: current.metadata.uid, resourceVersion: current.metadata.resourceVersion, labels: patch.labels } }),
      });
    }
    success.value = t("operations.runnerLabelsSaved", { name });
    labelRunner.value = null;
    await loadRunners();
  } catch (error) { labelsError.value = errorTranslationKey(error, "operations.runnerLabelsFailed"); }
  finally { labelsSaving.value = false; }
}
function openEviction(runner: Runner): void {
  if (!canManageRunners.value || evictionSaving.value || removing.value || labelRunner.value) return;
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
function openRemoval(runner: Runner): void {
  if (!canManageRunners.value || evictionSaving.value || removalSaving.value || evicting.value || labelRunner.value ||
    !runner.metadata?.name || !runner.metadata.uid || !removableRunner(runner)) return;
  removing.value = runner;
  removalError.value = "";
  success.value = "";
}
function closeRemoval(): void { if (!removalSaving.value) removing.value = null; }
async function confirmRemoval(): Promise<void> {
  const selected = removing.value;
  const name = selected?.metadata?.name;
  const uid = selected?.metadata?.uid;
  if (!canManageRunners.value || removalSaving.value || !name || !uid || !selected || !removableRunner(selected)) return;
  removalSaving.value = true;
  removalError.value = "";
  try {
    const path = `/apis/ebs/v1/runners/${encodeURIComponent(name)}`;
    const current = await request<Runner>(path);
    if (current.metadata?.uid !== uid || !current.metadata.resourceVersion) {
      removalError.value = "errors.conflict";
      return;
    }
    if (!removableRunner(current)) {
      removing.value = current;
      removalError.value = "operations.removeRunnerStateChanged";
      await loadRunners();
      return;
    }
    await request<unknown>(path, {
      method: "DELETE",
      body: JSON.stringify({ preconditions: { uid, resourceVersion: current.metadata.resourceVersion } }),
    });
    removing.value = null;
    success.value = t("operations.runnerRemoved", { name });
    runners.value = runners.value.filter(runner => runner.metadata?.uid !== uid);
    await loadRunners();
  } catch (error) { removalError.value = errorTranslationKey(error, "operations.removeRunnerFailed"); }
  finally { removalSaving.value = false; }
}
async function loadResources(): Promise<void> {
  resourcesLoading.value = true;
  resourcesError.value = "";
  try {
    const config = await request<Config>(resourcePath("build-resource"));
    editing.value = { config, content: parse(config.spec.content, { uniqueKeys: true }) as BuildResourceContent };
    resetEditor();
  } catch (error) { resourcesError.value = errorTranslationKey(error, "operations.loadResourcesFailed"); }
  finally { resourcesLoading.value = false; }
}
function resetEditor(): void {
  if (!editing.value) return;
  contentSource.value = JSON.stringify(editing.value.content, null, 2);
  dialogError.value = "";
  editorRevision.value += 1;
}
function resourcePath(name: string): string { return `/apis/ebs/v1/configs/${encodeURIComponent(name)}`; }
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
    await loadResources();
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
watch(activeSection, loadSection, { immediate: true });
</script>

<style scoped>
.operations-project-form .runner-search-field { position: relative; width: min(680px, 100%); }
.runner-search-field input { padding-right: 44px; }
.runner-search-button { position: absolute; right: 4px; top: 4px; width: 34px; height: 34px; display: grid; place-items: center; padding: 7px; border: 0; border-radius: 5px; color: var(--blue); background: transparent; cursor: pointer; }
.runner-search-button:hover:not(:disabled) { background: #edf6ff; }
.runner-search-button:focus-visible { outline: 2px solid var(--blue); outline-offset: 1px; }
.runner-search-button:disabled { opacity: .5; cursor: default; }
.runner-search-button svg { width: 18px; height: 18px; }
.runner-search-suggestions { position: absolute; top: calc(100% + 4px); left: 0; right: 0; z-index: 20; max-height: 240px; overflow-y: auto; margin: 0; padding: 4px; list-style: none; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); box-shadow: var(--shadow); }
.runner-search-suggestions li { padding: 8px 10px; border-radius: 4px; font-size: 14px; line-height: 1.5; overflow-wrap: anywhere; cursor: pointer; }
.runner-search-suggestions li:hover, .runner-search-suggestions li.active { color: var(--blue); background: #edf6ff; }
.runner-capacity { white-space: nowrap; font-variant-numeric: tabular-nums; }
.runner-type-arch { white-space: nowrap; }
.runner-label-row { display: flex; align-items: end; gap: 12px; flex-wrap: wrap; }
.runner-label-row .field { flex: 1; min-width: 180px; }
.runner-agent-version { display: inline-block; max-width: 12ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; vertical-align: middle; }
.runner-refresh-button { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.runner-refresh-button :deep(svg) { width: 16px; height: 16px; display: block; }
</style>
