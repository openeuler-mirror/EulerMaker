<template>
  <section ref="fullscreenElement" class="inline-job-log" :aria-label="t('jobLog.title')">
    <div v-if="logState || (truncated && logText)" class="inline-job-log-toolbar">
      <div class="job-log-summary">
        <span v-if="logState">{{ t('jobLog.state') }}: {{ logState }}</span>
        <span v-if="truncated && logText" class="job-log-notice">{{ t('jobLog.tailNotice') }}</span>
      </div>
    </div>
    <div class="job-log-box">
      <div class="job-log-controls">
        <button class="job-log-control-button" type="button" :aria-label="t('jobLog.refresh')" :title="t('jobLog.refresh')" :disabled="loading" @click="load"><Refresh /></button>
        <button class="job-log-control-button" type="button" :aria-label="t(isFullscreen ? 'jobLog.exitFullscreen' : 'jobLog.fullscreen')" :title="t(isFullscreen ? 'jobLog.exitFullscreen' : 'jobLog.fullscreen')" @click="toggleFullscreen"><ScaleToOriginal v-if="isFullscreen" /><FullScreen v-else /></button>
      </div>
      <p v-if="error" class="inline-error" role="alert">{{ t('jobLog.loadFailed') }}</p>
      <p v-else-if="loading && !logText" class="config-empty">{{ t('jobLog.loading') }}</p>
      <p v-else-if="!logText" class="config-empty">{{ t('jobLog.empty') }}</p>
      <pre v-else class="job-log-output">{{ logText }}</pre>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { FullScreen, Refresh, ScaleToOriginal } from '@element-plus/icons-vue';
import { useElementFullscreen } from '@/composables/useElementFullscreen';
import { LOG_TAIL_BYTES } from '@/utils/logPreview';

const props = defineProps<{ project: string; jobName: string }>();
const { t } = useI18n();
const { element: fullscreenElement, isFullscreen, toggleFullscreen } = useElementFullscreen();
const logText = ref('');
const logState = ref('');
const truncated = ref(false);
const loading = ref(false);
const error = ref(false);
let controller: AbortController | null = null;
let generation = 0;
let refreshTimer: ReturnType<typeof setInterval> | undefined;

watch(() => [props.project, props.jobName], () => {
  controller?.abort();
  generation += 1;
  loading.value = false;
  logText.value = '';
  logState.value = '';
  truncated.value = false;
  error.value = false;
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
  if (loading.value || !props.project || !props.jobName) return;
  const current = ++generation;
  const active = new AbortController();
  controller = active;
  loading.value = true;
  error.value = false;
  const path = `/artifacts/v1/projects/${encodeURIComponent(props.project)}/jobs/${encodeURIComponent(props.jobName)}`;
  try {
    const response = await fetch(`${path}/logs/content?stream=combined`, {
      headers: { Range: `bytes=-${LOG_TAIL_BYTES}` }, cache: 'no-store', signal: active.signal,
    });
    if (response.status === 404 || response.status === 416) {
      if (current === generation) { logText.value = ''; logState.value = ''; truncated.value = false; }
    } else {
      if (!response.ok) throw new Error(`log request failed: ${response.status}`);
      const content = await response.text();
      if (current === generation) {
        logText.value = content;
        logState.value = response.headers.get('X-Log-State') || '';
        truncated.value = Number(response.headers.get('X-Committed-Bytes') || 0) > LOG_TAIL_BYTES;
      }
    }
  } catch {
    if (current === generation && !active.signal.aborted) error.value = true;
  } finally {
    if (current === generation) loading.value = false;
  }
}
</script>
