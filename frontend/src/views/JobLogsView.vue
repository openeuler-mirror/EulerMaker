<template>
  <div class="detail-toolbar">
    <RouterLink class="back-link" :to="{ name: 'project', params: { name: project } }"><ArrowLeft /> {{ t('jobLog.back') }}</RouterLink>
    <button class="secondary-button" type="button" :disabled="loading" @click="load">{{ t('jobLog.refresh') }}</button>
  </div>

  <section class="page-heading">
    <div><h1>{{ t('jobLog.title') }}</h1><p class="job-log-identity">{{ project }} / {{ jobName }}</p></div>
    <StatusBadge v-if="job?.status?.phase" :value="job.status.phase" />
  </section>

  <section class="content-panel job-log-panel">
    <p v-if="jobErrorKey" class="inline-error" role="alert">{{ t(jobErrorKey) }}</p>
    <template v-else>
      <div v-if="job" class="job-log-meta">
        <span>Runner: {{ job.status?.runner || t('common.emptyValue') }}</span>
        <span v-if="logState">{{ t('jobLog.state') }}: {{ logState }}</span>
      </div>
      <p v-if="logError" class="inline-error" role="alert">{{ t('jobLog.loadFailed') }}</p>
      <p v-else-if="loading && !logText" class="config-empty">{{ t('jobLog.loading') }}</p>
      <p v-else-if="!logText" class="config-empty">{{ t('jobLog.empty') }}</p>
      <template v-else>
        <p v-if="truncated" class="job-log-notice">{{ t('jobLog.tailNotice') }}</p>
        <pre class="job-log-output">{{ logText }}</pre>
      </template>
    </template>
  </section>
</template>

<script setup lang="ts">
import { ArrowLeft } from '@element-plus/icons-vue';
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { RouterLink, useRoute } from 'vue-router';
import { useI18n } from 'vue-i18n';

import { errorTranslationKey, request } from '@/api';
import StatusBadge from '@/components/StatusBadge.vue';
import type { Job } from '@/types';

const route = useRoute();
const { t } = useI18n();
const project = computed(() => String(route.params.name || ''));
const jobName = computed(() => String(route.params.job || ''));
const job = ref<Job | null>(null);
const logText = ref('');
const logState = ref('');
const truncated = ref(false);
const loading = ref(false);
const logError = ref(false);
const jobErrorKey = ref('');
let controller: AbortController | null = null;
let generation = 0;
let refreshTimer: ReturnType<typeof setInterval> | undefined;

watch([project, jobName], () => {
  controller?.abort();
  generation += 1;
  loading.value = false;
  job.value = null;
  logText.value = '';
  logState.value = '';
  truncated.value = false;
  logError.value = false;
  jobErrorKey.value = '';
  void load();
}, { immediate: true });

onMounted(() => {
  refreshTimer = setInterval(() => {
    if (logState.value !== 'Completed' && !loading.value) void load();
  }, 5000);
});
onBeforeUnmount(() => {
  if (refreshTimer) clearInterval(refreshTimer);
  controller?.abort();
  generation += 1;
});

async function load(): Promise<void> {
  if (loading.value || !project.value || !jobName.value) return;
  const current = ++generation;
  const active = new AbortController();
  controller = active;
  loading.value = true;
  jobErrorKey.value = '';
  logError.value = false;
  const path = `/artifacts/v1/projects/${encodeURIComponent(project.value)}/jobs/${encodeURIComponent(jobName.value)}`;
  try {
    job.value = await request<Job>(`/apis/ebs/v1/projects/${encodeURIComponent(project.value)}/jobs/${encodeURIComponent(jobName.value)}`, { signal: active.signal });
  } catch (error) {
    if (current === generation && !active.signal.aborted) jobErrorKey.value = errorTranslationKey(error);
    if (current === generation) loading.value = false;
    return;
  }
  try {
    const response = await fetch(`${path}/logs/content?stream=combined`, {
      headers: { Range: 'bytes=-262144' }, cache: 'no-store', signal: active.signal,
    });
    if (response.status === 404 || response.status === 416) {
      if (current === generation) { logText.value = ''; logState.value = ''; truncated.value = false; }
    } else {
      if (!response.ok) throw new Error(`log request failed: ${response.status}`);
      const content = await response.text();
      if (current === generation) {
        logText.value = content;
        logState.value = response.headers.get('X-Log-State') || '';
        truncated.value = Number(response.headers.get('X-Committed-Bytes') || 0) > 262144;
      }
    }
  } catch {
    if (current === generation && !active.signal.aborted) logError.value = true;
  } finally {
    if (current === generation) loading.value = false;
  }
}
</script>
