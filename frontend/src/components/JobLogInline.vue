<template>
  <section class="inline-job-log" :aria-label="t('jobLog.title')">
    <div class="inline-job-log-toolbar">
      <span v-if="logState">{{ t('jobLog.state') }}: {{ logState }}</span>
      <button class="text-button" type="button" :disabled="loading" @click="load">{{ t('jobLog.refresh') }}</button>
    </div>
    <p v-if="error" class="inline-error" role="alert">{{ t('jobLog.loadFailed') }}</p>
    <p v-else-if="loading && !logText" class="config-empty">{{ t('jobLog.loading') }}</p>
    <p v-else-if="!logText" class="config-empty">{{ t('jobLog.empty') }}</p>
    <template v-else>
      <p v-if="truncated" class="job-log-notice">{{ t('jobLog.tailNotice') }}</p>
      <pre class="job-log-output">{{ logText }}</pre>
    </template>
  </section>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

const props = defineProps<{ project: string; jobName: string }>();
const { t } = useI18n();
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
    if (current === generation && !active.signal.aborted) error.value = true;
  } finally {
    if (current === generation) loading.value = false;
  }
}
</script>
