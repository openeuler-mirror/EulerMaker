<template>
  <section class="content-panel build-info-panel">
    <div class="section-heading">
      <h2>{{ t('project.buildInfo') }}</h2>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
    </div>
    <p v-if="loading && !buildInfo" class="config-empty">{{ t('project.buildInfoLoading') }}</p>
    <p v-else-if="missing" class="config-empty">{{ t('project.buildInfoMissing') }}</p>
    <p v-else-if="errorKey" class="inline-error" role="alert">{{ t(errorKey) }}</p>
    <template v-if="buildInfo">
      <dl class="detail-list build-detail-list">
        <div><dt>{{ t('project.status') }}</dt><dd class="build-info-status-row"><StatusBadge :value="buildInfo.status?.phase" /><span class="status-badge success"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoSucceededCount') }} {{ specBuildCounts.succeeded }}</span><span class="status-badge danger"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoFailedCount') }} {{ specBuildCounts.failed }}</span><span class="status-badge"><span class="status-dot" aria-hidden="true"></span>{{ t('project.buildInfoArchUnsupportedCount') }} {{ specBuildCounts.archUnsupported }}</span></dd></div>
        <div><dt>{{ t('project.buildInfoFailedPackages') }}</dt><dd>{{ buildInfo.status?.failedPackages?.join(', ') || t('common.emptyValue') }}</dd></div>
      </dl>
      <section class="build-detail-section">
        <h3>{{ t('project.buildInfoSpecStatus') }}</h3>
        <div v-if="specRows.length" class="spec-list-toolbar"><label class="search-box"><Search /><input v-model="specSearch" type="search" :placeholder="t('project.searchSpecs')" :aria-label="t('project.searchSpecs')" /></label></div>
        <p v-if="!specRows.length" class="config-empty">{{ t('project.buildInfoNoSpecs') }}</p>
        <p v-else-if="!filteredSpecRows.length" class="config-empty">{{ t('project.noMatchingSpecs') }}</p>
        <div v-else class="project-table-wrap"><table class="project-table">
          <thead><tr><th>Spec</th><th><button ref="statusFilterTrigger" class="spec-status-filter-trigger" type="button" :aria-expanded="statusFilterOpen" :aria-controls="statusFilterOpen ? statusFilterId : undefined" @click="toggleStatusFilter">{{ t('project.buildInfoBuildStatus') }}<Filter aria-hidden="true" /><span v-if="selectedSpecStatuses.length" class="spec-status-filter-count">{{ selectedSpecStatuses.length }}</span></button></th></tr></thead>
          <tbody><template v-for="row in paginatedSpecRows" :key="row.name">
            <tr><td><button class="spec-expand-button" type="button" :aria-expanded="selectedSpecName === row.name" @click="selectedSpecName = selectedSpecName === row.name ? '' : row.name">{{ row.name }}</button></td><td><StatusBadge :value="row.displayStatus" /></td></tr>
            <tr v-if="selectedSpecName === row.name" class="spec-jobs-row"><td colspan="2"><ProjectJobs :project="project" :build-name="buildName" :spec-name="row.name" :can-abort="canAbortJobs" /></td></tr>
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
import { ArrowLeft, ArrowRight, Check, Filter, Search } from '@element-plus/icons-vue';
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, watch, type CSSProperties } from 'vue';
import { useI18n } from 'vue-i18n';

import { ApiError, errorTranslationKey, request } from '@/api';
import type { BuildInfo } from '@/types';
import AppSelect from './AppSelect.vue';
import ProjectJobs from './ProjectJobs.vue';
import StatusBadge from './StatusBadge.vue';

const props = defineProps<{ project: string; buildName: string; canAbortJobs: boolean }>();
const { t, te } = useI18n();
const buildInfo = ref<BuildInfo | null>(null);
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
const specRows = computed(() => Object.entries(buildInfo.value?.status?.specStatus?.build || {})
  .map(([name, value]) => ({
    name,
    buildStatus: value.status,
    displayStatus: value.status === 'Failed' && value.conditions?.some((condition) => condition.reason === 'ArchUnsupported')
      ? 'ArchUnsupported'
      : value.status === 'Failed' && value.conditions?.some((condition) => condition.reason === 'RpmDependsMissing')
        ? 'RpmDependsMissing' : value.status,
  }))
  .sort((left, right) => left.name.localeCompare(right.name)));
const specBuildCounts = computed(() => ({
  succeeded: specRows.value.filter((row) => row.buildStatus === 'Succeeded').length,
  failed: specRows.value.filter((row) => row.buildStatus === 'Failed' && row.displayStatus !== 'ArchUnsupported').length,
  archUnsupported: specRows.value.filter((row) => row.displayStatus === 'ArchUnsupported').length,
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
    (!query || row.name.toLowerCase().includes(query)) &&
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
  controller?.abort();
  if (!props.project || !props.buildName) return;
  statusFilterOpen.value = false;
  specCurrentPage.value = 1;
  selectedSpecName.value = '';
  const current = new AbortController();
  controller = current;
  buildInfo.value = null;
  loading.value = true;
  missing.value = false;
  errorKey.value = '';
  try {
    const path = `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/buildinfos/${encodeURIComponent(props.buildName)}?includeFields=status.phase,status.failedPackages,status.conditions,status.specStatus.build`;
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
</script>
