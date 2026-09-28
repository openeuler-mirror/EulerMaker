<template>
  <div class="package-job-artifacts">
    <div class="package-job-artifacts-heading">
      <strong>{{ t('project.jobRPMs') }}</strong>
    </div>
    <p v-if="!succeeded" class="config-empty">{{ t('project.noJobRPMs') }}</p>
    <p v-else-if="loading" class="config-empty">{{ t('project.loadingJobRPMs') }}</p>
    <div v-else-if="errorKey" class="inline-error compact-error" role="alert"><span>{{ t(errorKey) }}</span><button type="button" @click="load">{{ t('common.reload') }}</button></div>
    <p v-else-if="!rpms.length" class="config-empty">{{ t('project.noJobRPMs') }}</p>
    <ul v-else class="package-job-artifact-list">
      <li v-for="rpm in rpms" :key="rpm.id"><a :href="`/artifacts/v1/artifacts/${encodeURIComponent(rpm.id)}/content`">{{ rpm.fileName }}</a></li>
    </ul>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import { errorTranslationKey, request } from '@/api';

type Artifact = { id: string; fileName: string; category: string; state: string };

const props = defineProps<{ project: string; jobName: string; succeeded: boolean }>();
const { t } = useI18n();
const rpms = ref<Artifact[]>([]);
const loading = ref(false);
const errorKey = ref('');
let controller: AbortController | null = null;

onMounted(() => { if (props.succeeded) void load(); });
onBeforeUnmount(() => controller?.abort());

async function load(): Promise<void> {
  controller?.abort();
  const active = new AbortController();
  controller = active;
  loading.value = true;
  errorKey.value = '';
  try {
    const path = `/artifacts/v1/projects/${encodeURIComponent(props.project)}/jobs/${encodeURIComponent(props.jobName)}/artifacts?category=artifact`;
    const response = await request<{ items: Artifact[] }>(path, { signal: active.signal });
    if (controller === active) {
      rpms.value = (response.items || [])
        .filter((item) => item.category === 'artifact' && item.state === 'Completed' && item.fileName.toLowerCase().endsWith('.rpm'))
        .sort((left, right) => left.fileName.localeCompare(right.fileName));
    }
  } catch (error) {
    if (controller === active && !active.signal.aborted) errorKey.value = errorTranslationKey(error, 'project.loadJobRPMsFailed');
  } finally {
    if (controller === active) loading.value = false;
  }
}
</script>
