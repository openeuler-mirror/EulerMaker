<template>
  <section class="content-panel operations-section">
    <div class="section-heading"><h2>{{ t('jobControl.title') }}</h2><button class="secondary-button" :disabled="loading || busy" @click="load('')">{{ t('common.refresh') }}</button></div>
    <label class="field"><span>{{ t('jobControl.phase') }}</span><select v-model="phase" :disabled="busy"><option value="">{{ t('jobControl.all') }}</option><option>Pending</option><option>Running</option></select></label>
    <p v-if="message" role="status">{{ message }}</p><p v-if="error" class="inline-error" role="alert">{{ error }}</p>
    <div class="project-table-wrap"><table class="project-table"><thead><tr><th>{{ t('jobControl.name') }}</th><th>{{ t('jobControl.phase') }}</th><th>Runner</th><th>{{ t('admin.actions') }}</th></tr></thead>
      <tbody><tr v-for="job in jobs" :key="job.metadata?.uid"><td>{{ job.metadata?.name }}</td><td><StatusBadge :value="job.status?.phase" /></td><td>{{ job.status?.runner || '—' }}</td><td><button v-if="canAbort && abortable(job)" class="text-button danger-link" :disabled="busy || !job.metadata?.uid" @click="open(job)">{{ t('jobControl.abort') }}</button></td></tr></tbody>
    </table></div>
    <button v-if="next" class="secondary-button" :disabled="loading || busy" @click="load(next)">{{ t('jobControl.next') }}</button>
    <ModalDialog v-if="selected" title-id="abort-job-title" :title="t('jobControl.abort')" :close-label="t('common.close')" @close="close">
      <form class="project-form" @submit.prevent="abort">
        <p>{{ project }} / {{ selected.metadata?.name }} · {{ selected.status?.phase }}</p>
        <label class="field"><span>{{ t('jobControl.reason') }}</span><textarea v-model="reason" maxlength="1024" :disabled="busy" /></label>
        <p class="form-hint">{{ t('jobControl.hint') }}</p><p v-if="dialogError" class="inline-error" role="alert">{{ dialogError }}</p>
        <div class="modal-actions"><button type="button" class="secondary-button" :disabled="busy" @click="close">{{ t('common.cancel') }}</button><button class="primary-button danger-button" :disabled="busy">{{ t('jobControl.abort') }}</button></div>
      </form>
    </ModalDialog>
  </section>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { ApiError, errorTranslationKey, list, request } from '@/api';
import type { Job } from '@/types';
import ModalDialog from './ModalDialog.vue';
import StatusBadge from './StatusBadge.vue';

const props = defineProps<{ project: string; canAbort: boolean }>();
const { t } = useI18n();
const jobs = ref<Job[]>([]), phase = ref('Running'), next = ref('');
const loading = ref(false), busy = ref(false), error = ref(''), message = ref('');
const selected = ref<Job | null>(null), reason = ref(''), dialogError = ref('');
let generation = 0;
const path = (project: string) => `/apis/ebs/v1/projects/${encodeURIComponent(project)}/jobs`;
const abortable = (job: Job) => job.status?.phase === 'Pending' || job.status?.phase === 'Running';
watch(() => [props.project, phase.value], () => { selected.value = null; message.value = ''; void load(''); }, { immediate: true });
async function load(cursor: string): Promise<void> {
  const current = ++generation;
  if (!props.project) { jobs.value = []; next.value = ''; loading.value = false; return; }
  loading.value = true; error.value = '';
  try {
    const query = new URLSearchParams({ limit: '50' });
    if (phase.value) query.set('fieldSelector', `status.phase=${phase.value}`);
    if (cursor) query.set('continue', cursor);
    const page = await list<Job>(`${path(props.project)}?${query}`);
    if (current === generation) { jobs.value = page.items; next.value = page.next; }
  } catch (e) { if (current === generation) error.value = t(errorTranslationKey(e)); }
  finally { if (current === generation) loading.value = false; }
}
function open(job: Job): void { selected.value = job; reason.value = ''; dialogError.value = ''; }
function close(): void { if (!busy.value) selected.value = null; }
async function abort(): Promise<void> {
  const job = selected.value;
  if (busy.value || !props.canAbort || !job?.metadata?.uid || !job.metadata.name) return;
  const project = props.project, uid = job.metadata.uid;
  const url = `${path(project)}/${encodeURIComponent(job.metadata.name)}`;
  busy.value = true; dialogError.value = ''; message.value = '';
  try {
    let result: Job;
    try {
      result = await request<Job>(`${url}/abort`, { method: 'POST', body: JSON.stringify({ uid, reason: reason.value }), signal: AbortSignal.timeout(15000) });
    } catch (e) {
      if (e instanceof ApiError && e.status < 500 && e.status !== 408) throw e;
      result = await request<Job>(url, { signal: AbortSignal.timeout(15000) });
      if (result.metadata?.uid === uid && abortable(result)) { dialogError.value = t('jobControl.unknown'); return; }
    }
    if (result.metadata?.uid !== uid) { dialogError.value = t('errors.conflict'); return; }
    if (!['Aborted', 'Succeeded', 'Failed'].includes(result.status?.phase || '')) { dialogError.value = t('jobControl.unknown'); return; }
    if (props.project === project) {
      message.value = t(result.status?.phase === 'Aborted' ? 'jobControl.aborted' : 'jobControl.finished');
      selected.value = null; await load('');
    }
  } catch (e) { dialogError.value = t(errorTranslationKey(e)); }
  finally { busy.value = false; }
}
</script>
