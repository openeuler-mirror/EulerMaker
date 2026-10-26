<template>
  <ModalDialog title-id="package-yaml-title" :title="t('project.packageYamlTitle')" :close-label="t('common.close')" @close="close">
    <form class="project-form" @submit.prevent="save">
      <p class="form-hint">{{ t('project.packageYamlHint') }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <label v-else-if="loadedProject" class="field"><span>spec.packageRepos</span><textarea v-model="source" class="yaml-editor" spellcheck="false" :disabled="saving" :aria-label="t('project.packageYamlTitle')"></textarea></label>
      <div v-if="error" class="form-error" role="alert">{{ t(error) }}</div>
      <div class="modal-actions">
        <button v-if="!loading && !loadedProject" class="secondary-button" type="button" @click="load">{{ t('common.reload') }}</button>
        <button class="secondary-button" type="button" :disabled="saving" @click="close">{{ t('common.cancel') }}</button>
        <button class="primary-button" type="submit" :disabled="loading || saving || !loadedProject">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
      </div>
    </form>
  </ModalDialog>
</template>
<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { parse, stringify } from 'yaml';
import { ApiError, errorTranslationKey, request } from '@/api';
import type { Project } from '@/types';
import { validatePackageRepos } from '@/utils/packageReposYaml';
import ModalDialog from './ModalDialog.vue';
const props = defineProps<{ projectName: string }>();
const emit = defineEmits<{ close: []; saved: [project: Project] }>();
const { t } = useI18n();
const loadedProject = ref<Project | null>(null);
const source = ref('');
const error = ref('');
const loading = ref(false);
const saving = ref(false);
const controller = new AbortController();
const path = `/apis/ebs/v1/projects/${encodeURIComponent(props.projectName)}`;
function close(): void { if (!saving.value) emit('close'); }
async function load(): Promise<void> {
  loading.value = true;
  error.value = '';
  try {
    const project = await request<Project>(path, { signal: controller.signal });
    if (controller.signal.aborted) return;
    loadedProject.value = project;
    source.value = stringify(project.spec?.packageRepos || [], { lineWidth: 0 });
  } catch (reason) {
    if (!controller.signal.aborted) error.value = errorTranslationKey(reason, 'errors.loadProject');
  } finally { loading.value = false; }
}
async function save(): Promise<void> {
  if (saving.value || !loadedProject.value || loading.value) return;
  error.value = '';
  let packageRepos;
  try {
    packageRepos = validatePackageRepos(parse(source.value, { uniqueKeys: true }));
  } catch (reason) {
    error.value = reason instanceof Error && reason.message.startsWith('project.') ? reason.message : 'project.packageYamlInvalid';
    return;
  }
  saving.value = true;
  try {
    const original = loadedProject.value;
    const updated = await request<Project>(path, { method: 'PUT', signal: controller.signal, body: JSON.stringify({ ...original, spec: { ...original.spec, packageRepos } }) });
    if (!controller.signal.aborted) emit('saved', updated);
  } catch (reason) {
    if (!controller.signal.aborted) error.value = reason instanceof ApiError && reason.status === 409 ? 'project.packageYamlConflict' : errorTranslationKey(reason, 'errors.updateProject');
  } finally { saving.value = false; }
}
onMounted(load);
onBeforeUnmount(() => controller.abort());
</script>
