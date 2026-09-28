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
        <div><dt>{{ t('project.status') }}</dt><dd><StatusBadge :value="buildInfo.status?.phase" /></dd></div>
        <div><dt>{{ t('project.buildInfoFailedPackages') }}</dt><dd>{{ buildInfo.status?.failedPackages?.join(', ') || t('common.emptyValue') }}</dd></div>
      </dl>
      <section class="build-detail-section">
        <h3>{{ t('project.buildInfoSpecStatus') }}</h3>
        <div v-if="specRows.length" class="spec-list-toolbar"><label class="search-box"><Search /><input v-model="specSearch" type="search" :placeholder="t('project.searchSpecs')" :aria-label="t('project.searchSpecs')" /></label></div>
        <p v-if="!specRows.length" class="config-empty">{{ t('project.buildInfoNoSpecs') }}</p>
        <p v-else-if="!filteredSpecRows.length" class="config-empty">{{ t('project.noMatchingSpecs') }}</p>
        <div v-else class="project-table-wrap"><table class="project-table">
          <thead><tr><th>Spec</th><th>{{ t('project.buildInfoBuildStatus') }}</th></tr></thead>
          <tbody><template v-for="row in filteredSpecRows" :key="row.name">
            <tr><td><button class="spec-expand-button" type="button" :aria-expanded="selectedSpecName === row.name" @click="selectedSpecName = selectedSpecName === row.name ? '' : row.name">{{ row.name }}</button></td><td><StatusBadge :value="row.buildStatus" /></td></tr>
            <tr v-if="selectedSpecName === row.name" class="spec-jobs-row"><td colspan="2"><ProjectJobs :project="project" :build-name="buildName" :spec-name="row.name" :can-abort="canAbortJobs" /></td></tr>
          </template></tbody>
        </table></div>
      </section>
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
import { Search } from '@element-plus/icons-vue';
import { computed, onBeforeUnmount, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import { ApiError, errorTranslationKey, request } from '@/api';
import type { BuildInfo } from '@/types';
import ProjectJobs from './ProjectJobs.vue';
import StatusBadge from './StatusBadge.vue';

const props = defineProps<{ project: string; buildName: string; canAbortJobs: boolean }>();
const { t } = useI18n();
const buildInfo = ref<BuildInfo | null>(null);
const loading = ref(false);
const missing = ref(false);
const errorKey = ref('');
const selectedSpecName = ref('');
const specSearch = ref('');
const specRows = computed(() => Object.entries(buildInfo.value?.status?.specStatus || {})
  .map(([name, value]) => ({ name, buildStatus: value.build?.status }))
  .sort((left, right) => left.name.localeCompare(right.name)));
const filteredSpecRows = computed(() => {
  const query = specSearch.value.trim().toLowerCase();
  return query ? specRows.value.filter((row) => row.name.toLowerCase().includes(query)) : specRows.value;
});
let controller: AbortController | null = null;

watch(() => [props.project, props.buildName], () => {
  buildInfo.value = null;
  selectedSpecName.value = '';
  specSearch.value = '';
  void load();
}, { immediate: true });
onBeforeUnmount(() => controller?.abort());

async function load(): Promise<void> {
  controller?.abort();
  if (!props.project || !props.buildName) return;
  const current = new AbortController();
  controller = current;
  buildInfo.value = null;
  loading.value = true;
  missing.value = false;
  errorKey.value = '';
  try {
    const path = `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/buildinfos/${encodeURIComponent(props.buildName)}`;
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
