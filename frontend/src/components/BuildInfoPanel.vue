<template>
  <section class="content-panel build-info-panel">
    <div class="section-heading">
      <h2>{{ t('project.buildInfo') }}</h2>
      <button class="secondary-button compact-button" type="button" :disabled="loading || snapshotLoading"
        :title="t('common.refresh')" :aria-label="t('common.refresh')" @click="load"><Refresh aria-hidden="true" /></button>
    </div>
    <dl v-if="build?.spec?.buildType && build.spec.buildType !== 'full'" class="detail-list build-detail-list">
      <div class="build-selected-packages-row"><dt>{{ t('project.buildInfoSelectedPackages') }}</dt><dd class="value-chip-list">
        <template v-if="selectedPackages.length">
          <code v-for="(item, index) in visibleSelectedPackages" :key="`${item}-${index}`">{{ item }}</code>
          <button v-if="selectedPackages.length > 10" class="text-button" type="button" :aria-expanded="selectedPackagesExpanded" @click="selectedPackagesExpanded = !selectedPackagesExpanded">{{ t(selectedPackagesExpanded ? 'project.collapsePackageList' : 'project.expandPackageList', { count: selectedPackages.length }) }}</button>
        </template>
        <span v-else>{{ t('common.emptyValue') }}</span>
      </dd></div>
    </dl>
    <p v-if="loading && !buildInfo" class="config-empty">{{ t('project.buildInfoLoading') }}</p>
    <p v-else-if="missing" class="config-empty">{{ t('project.buildInfoMissing') }}</p>
    <p v-else-if="errorKey" class="inline-error" role="alert">{{ t(errorKey) }}</p>
    <template v-if="buildInfo">
      <dl class="detail-list build-detail-list">
        <div><dt>{{ t('project.status') }}</dt><dd class="build-info-status-row"><StatusBadge :value="buildInfo.status?.phase" /><span class="status-badge success"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoSucceededCount') }} {{ specBuildCounts.succeeded }}</span><span class="status-badge danger"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoFailedCount') }} {{ specBuildCounts.failed }}</span><span class="status-badge"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoArchUnsupportedCount') }} {{ specBuildCounts.archUnsupported }}</span></dd></div>
        <div><dt>{{ t('project.buildInfoFailedPackages') }}</dt><dd class="build-info-failed-packages"><template v-if="failedPackages.length">
          <span v-for="(item, index) in visibleFailedPackages" :key="`${item}-${index}`" class="status-badge">{{ item }}</span>
          <button v-if="failedPackages.length > 10" class="text-button" type="button" :aria-expanded="failedPackagesExpanded" @click="failedPackagesExpanded = !failedPackagesExpanded">{{ t(failedPackagesExpanded ? 'project.collapsePackageList' : 'project.expandPackageList', { count: failedPackages.length }) }}</button>
        </template><span v-else>{{ t('common.emptyValue') }}</span></dd></div>
      </dl>
      <section class="build-detail-section">
        <h3>{{ t('project.buildInfoSpecStatus') }}</h3>
        <p v-if="snapshotErrorKey" class="inline-error" role="alert">{{ t(snapshotErrorKey) }}</p>
        <div v-if="specRows.length" class="spec-list-toolbar"><label class="search-box"><Search /><input v-model="specSearch" type="search" :placeholder="t('project.searchSpecs')" :aria-label="t('project.searchSpecs')" /></label></div>
        <p v-if="!specRows.length" class="config-empty">{{ t('project.buildInfoNoSpecs') }}</p>
        <p v-else-if="!filteredSpecRows.length" class="config-empty">{{ t('project.noMatchingSpecs') }}</p>
        <div v-else class="project-table-wrap"><table class="project-table">
          <thead><tr><th>Spec</th><th>{{ t('project.buildInfoPackageName') }}</th><th>Commit ID</th><th><button ref="statusFilterTrigger" class="spec-status-filter-trigger" type="button" :aria-expanded="statusFilterOpen" :aria-controls="statusFilterOpen ? statusFilterId : undefined" @click="toggleStatusFilter">{{ t('project.buildInfoBuildStatus') }}<Filter aria-hidden="true" /><span v-if="selectedSpecStatuses.length" class="spec-status-filter-count">{{ selectedSpecStatuses.length }}</span></button></th><th>{{ t('project.buildInfoDiagnostics') }}</th></tr></thead>
          <tbody><template v-for="row in paginatedSpecRows" :key="row.key">
            <tr><td><button v-if="row.specName" class="spec-expand-button" type="button" :aria-expanded="selectedSpecName === row.key" @click="selectedSpecName = selectedSpecName === row.key ? '' : row.key">{{ row.name }}</button><span v-else :title="row.message">{{ row.name }}</span></td><td>{{ row.packageName || t('common.emptyValue') }}</td><td><code v-if="row.commitID" :title="row.commitID">{{ row.commitID.slice(0, 12) }}</code><span v-else>{{ snapshotLoading ? '…' : t('common.emptyValue') }}</span></td><td><StatusBadge :value="row.displayStatus" /></td>
              <td class="spec-diagnostics">
                <template v-if="row.reason || row.message">
                  <strong v-if="row.reason">{{ row.reason }}</strong>
                  <p v-if="row.message">{{ row.message }}</p>
                </template>
                <span v-else>{{ t('common.emptyValue') }}</span>
              </td>
            </tr>
            <tr v-if="row.specName && selectedSpecName === row.key" class="spec-jobs-row"><td colspan="5"><ProjectJobs :project="project" :build-name="buildName" :spec-name="row.specName" :can-abort="canAbortJobs" /></td></tr>
          </template></tbody>
        </table></div>
        <div v-if="filteredSpecRows.length" class="table-footer">
          <div class="page-summary">
            <span>{{ t('common.count', { count: filteredSpecRows.length }) }}</span>
            <AppSelect :model-value="String(specPageSize)" :options="specPageSizes.map((size) => ({ value: String(size), label: t('common.itemsPerPage', { count: size }) }))" :label="t('common.perPage')" compact @change="changeSpecPageSize" />
            <nav class="pagination-row" :aria-label="t('common.pagination')">
              <button class="page-button arrow-button" type="button" :aria-label="t('common.previous')" :disabled="specCurrentPage === 1" @click="changeSpecPage(specCurrentPage - 1)"><ArrowLeft /></button>
              <span class="page-button active" aria-current="page">{{ specCurrentPage }} / {{ specTotalPages }}</span>
              <button class="page-button arrow-button" type="button" :aria-label="t('common.next')" :disabled="specCurrentPage === specTotalPages" @click="changeSpecPage(specCurrentPage + 1)"><ArrowRight /></button>
            </nav>
          </div>
        </div>
      </section>
      <Teleport to="body">
        <div v-if="statusFilterOpen" :id="statusFilterId" ref="statusFilterMenu" class="spec-status-filter-menu" :style="statusFilterStyle" role="group" :aria-label="t('project.buildInfoBuildStatus')">
          <label class="spec-status-filter-option all" :class="{ selected: !selectedSpecStatuses.length }"><input type="checkbox" :checked="!selectedSpecStatuses.length" @change="clearSpecStatusFilter" /><span class="spec-status-filter-check" aria-hidden="true"><Check /></span><span>{{ t('project.allBuildStatuses') }}</span></label>
          <label v-for="option in specStatusOptions" :key="option.value" class="spec-status-filter-option" :class="{ selected: selectedSpecStatuses.includes(option.value) }"><input type="checkbox" :checked="selectedSpecStatuses.includes(option.value)" @change="toggleSpecStatus(option.value, $event)" /><span class="spec-status-filter-check" aria-hidden="true"><Check /></span><span>{{ option.label }}</span></label>
        </div>
      </Teleport>
      <section v-if="buildInfo.status?.conditions?.length" class="build-detail-section">
        <h3>{{ t('project.buildInfoConditions') }}</h3>
        <div v-for="condition in buildInfo.status.conditions" :key="condition.type" class="build-info-condition">
          <strong>{{ condition.type }} · {{ condition.status }}</strong>
          <span>{{ condition.reason || t('common.emptyValue') }}</span>
          <p v-if="condition.message">{{ condition.message }}</p>
        </div>
      </section>
    </template>
  </section>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, Check, Filter, Refresh, Search } from '@element-plus/icons-vue';
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, watch, type CSSProperties } from 'vue';
import { useI18n } from 'vue-i18n';

import { ApiError, errorTranslationKey, request } from '@/api';
import type { Build, BuildInfo, Snapshot } from '@/types';
import { buildInfoSpecRows } from '@/utils/buildInfo';
import AppSelect from './AppSelect.vue';
import ProjectJobs from './ProjectJobs.vue';
import StatusBadge from './StatusBadge.vue';

const props = defineProps<{ project: string; buildName: string; canAbortJobs: boolean; build?: Build }>();
const { t, te } = useI18n();
const buildInfo = ref<BuildInfo | null>(null);
const snapshot = ref<Snapshot | null>(null);
const snapshotLoading = ref(false);
const snapshotErrorKey = ref('');
const loading = ref(false);
const missing = ref(false);
const errorKey = ref('');
const selectedSpecName = ref('');
const specSearch = ref('');
const selectedSpecStatuses = ref<string[]>([]);
const statusFilterOpen = ref(false);
const statusFilterTrigger = ref<HTMLElement | null>(null);
const statusFilterMenu = ref<HTMLElement | null>(null);
const statusFilterStyle = ref<CSSProperties>({});
const statusFilterId = useId();
const specPageSizes = [10, 20, 50] as const;
const specPageSize = ref<number>(20);
const specCurrentPage = ref(1);
const specRows = computed(() => buildInfoSpecRows(buildInfo.value, snapshot.value));
const failedPackages = computed(() => [...new Set(specRows.value
  .filter((row) => ['PrepareFailed', 'BuildFailed', 'InstallFailed', 'ArchUnsupported'].includes(row.buildStatus))
  .map((row) => row.packageName))].sort());
const selectedPackagesExpanded = ref(false);
const failedPackagesExpanded = ref(false);
const selectedPackages = computed(() => props.build?.spec?.packages || []);
const visibleSelectedPackages = computed(() => selectedPackagesExpanded.value ? selectedPackages.value : selectedPackages.value.slice(0, 10));
const visibleFailedPackages = computed(() => failedPackagesExpanded.value ? failedPackages.value : failedPackages.value.slice(0, 10));
const specBuildCounts = computed(() => ({
  succeeded: specRows.value.filter((row) => row.buildStatus === 'Succeeded').length,
  failed: specRows.value.filter((row) => ['PrepareFailed', 'BuildFailed', 'InstallFailed'].includes(row.buildStatus)).length,
  archUnsupported: specRows.value.filter((row) => row.buildStatus === 'ArchUnsupported').length,
}));
const specStatusOptions = computed(() => [
  ...[...new Set(specRows.value.map((row) => row.displayStatus || 'Unknown'))]
    .sort((left, right) => left.localeCompare(right))
    .map((status) => {
      const key = `status.${status.toLowerCase()}`;
      return { value: status, label: status === 'Unknown' ? t('common.unknown') : te(key) ? t(key) : status };
    }),
]);
const filteredSpecRows = computed(() => {
  const query = specSearch.value.trim().toLowerCase();
  return specRows.value.filter((row) =>
    (!query || row.name.toLowerCase().includes(query) || row.packageName?.toLowerCase().includes(query)) &&
    (!selectedSpecStatuses.value.length || selectedSpecStatuses.value.includes(row.displayStatus || 'Unknown')));
});
const specTotalPages = computed(() => Math.max(1, Math.ceil(filteredSpecRows.value.length / specPageSize.value)));
const paginatedSpecRows = computed(() => filteredSpecRows.value.slice((specCurrentPage.value - 1) * specPageSize.value, specCurrentPage.value * specPageSize.value));
let controller: AbortController | null = null;

watch(specSearch, () => {
  specCurrentPage.value = 1;
  selectedSpecName.value = '';
});
watch(() => [props.project, props.buildName], () => {
  buildInfo.value = null;
  snapshot.value = null;
  selectedSpecName.value = '';
  specSearch.value = '';
  selectedSpecStatuses.value = [];
  statusFilterOpen.value = false;
  specCurrentPage.value = 1;
  void load();
}, { immediate: true });
onMounted(() => {
  window.addEventListener('pointerdown', closeStatusFilterOnOutsideClick);
  window.addEventListener('keydown', closeStatusFilterOnEscape);
  window.addEventListener('resize', positionStatusFilter);
  window.addEventListener('scroll', positionStatusFilter, true);
});
onBeforeUnmount(() => {
  controller?.abort();
  window.removeEventListener('pointerdown', closeStatusFilterOnOutsideClick);
  window.removeEventListener('keydown', closeStatusFilterOnEscape);
  window.removeEventListener('resize', positionStatusFilter);
  window.removeEventListener('scroll', positionStatusFilter, true);
});

function changeSpecPageSize(value: string): void {
  specPageSize.value = Number(value);
  specCurrentPage.value = 1;
  selectedSpecName.value = '';
}

function resetSpecPage(): void {
  specCurrentPage.value = 1;
  selectedSpecName.value = '';
}

function clearSpecStatusFilter(): void {
  selectedSpecStatuses.value = [];
  resetSpecPage();
}

function toggleSpecStatus(status: string, event: Event): void {
  const checked = (event.target as HTMLInputElement).checked;
  selectedSpecStatuses.value = checked
    ? [...selectedSpecStatuses.value, status]
    : selectedSpecStatuses.value.filter((value) => value !== status);
  resetSpecPage();
}

function toggleStatusFilter(): void {
  statusFilterOpen.value = !statusFilterOpen.value;
  if (statusFilterOpen.value) void nextTick(positionStatusFilter);
}

function positionStatusFilter(): void {
  if (!statusFilterOpen.value || !statusFilterTrigger.value) return;
  const rect = statusFilterTrigger.value.getBoundingClientRect();
  const width = 210;
  const height = Math.min(statusFilterMenu.value?.offsetHeight || 240, 240);
  const top = window.innerHeight - rect.bottom < height + 8 && rect.top > height
    ? rect.top - height - 4 : rect.bottom + 4;
  statusFilterStyle.value = {
    top: `${top}px`,
    left: `${Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))}px`,
  };
}

function closeStatusFilterOnOutsideClick(event: PointerEvent): void {
  const target = event.target as Node;
  if (!statusFilterTrigger.value?.contains(target) && !statusFilterMenu.value?.contains(target)) statusFilterOpen.value = false;
}

function closeStatusFilterOnEscape(event: KeyboardEvent): void {
  if (event.key !== 'Escape' || !statusFilterOpen.value) return;
  statusFilterOpen.value = false;
  statusFilterTrigger.value?.focus();
}

function changeSpecPage(page: number): void {
  if (page < 1 || page > specTotalPages.value) return;
  specCurrentPage.value = page;
  selectedSpecName.value = '';
}

async function load(): Promise<void> {
  selectedPackagesExpanded.value = false;
  failedPackagesExpanded.value = false;
  controller?.abort();
  if (!props.project || !props.buildName) return;
  statusFilterOpen.value = false;
  specCurrentPage.value = 1;
  selectedSpecName.value = '';
  const current = new AbortController();
  controller = current;
  buildInfo.value = null;
  snapshot.value = null;
  snapshotErrorKey.value = '';
  snapshotLoading.value = true;
  loading.value = true;
  missing.value = false;
  errorKey.value = '';
  void loadSnapshot(current);
  try {
    const path = `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/buildinfos/${encodeURIComponent(props.buildName)}?includeFields=status.phase,status.conditions,status.specResults`;
    const result = await request<BuildInfo>(path, { signal: current.signal });
    if (!current.signal.aborted) buildInfo.value = result;
  } catch (error) {
    if (!current.signal.aborted) {
      if (error instanceof ApiError && error.status === 404) missing.value = true;
      else errorKey.value = errorTranslationKey(error);
    }
  } finally {
    if (!current.signal.aborted) loading.value = false;
  }
}

async function loadSnapshot(current: AbortController): Promise<void> {
  const path = `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/snapshots/${encodeURIComponent(props.buildName)}?includeFields=status.packageRepoStatuses`;
  try {
    const result = await request<Snapshot>(path, { signal: current.signal });
    if (!current.signal.aborted) snapshot.value = result;
  } catch (error) {
    if (!current.signal.aborted) snapshotErrorKey.value = error instanceof ApiError && error.status === 404
      ? 'project.buildInfoSnapshotMissing' : 'project.buildInfoSnapshotLoadFailed';
  } finally {
    if (!current.signal.aborted) snapshotLoading.value = false;
  }
}
</script>

<style scoped>
.spec-diagnostics { min-width: 200px; max-width: 440px; white-space: normal; overflow-wrap: anywhere; }
.spec-diagnostics p { margin: 0; white-space: pre-wrap; color: var(--muted); font-size: 12px; line-height: 1.6; }
.spec-diagnostics strong + p { margin-top: 4px; }
</style>
