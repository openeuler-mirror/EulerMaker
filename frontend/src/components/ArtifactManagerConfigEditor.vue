<template>
  <section class="content-panel">
    <div class="section-heading artifact-config-heading">
      <h2>{{ t('artifactManagerConfig.title') }}</h2>
      <button class="icon-button artifact-config-refresh" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="loading || saving || editing" @click="load"><Refresh /></button>
    </div>
    <p class="form-hint">{{ t('artifactManagerConfig.hint') }}</p>
    <p v-if="error" class="form-error" role="alert">{{ t(error) }}</p>
    <p v-if="success" class="success-banner" role="status">{{ t('artifactManagerConfig.saved') }}</p>
    <div v-if="loading" class="skeleton-list" :aria-label="t('artifactManagerConfig.loading')"><span v-for="item in 3" :key="item"></span></div>
    <form v-else-if="current" class="project-form" @submit.prevent="save">
      <fieldset class="artifact-config-fields" :disabled="saving">
        <div class="artifact-config-actions">
          <button v-if="!editing" class="secondary-button" type="button" @click="startEdit">{{ t('common.edit') }}</button>
          <button v-else class="secondary-button" type="button" @click="addInstance">{{ t('artifactManagerConfig.addInstance') }}</button>
        </div>
        <section v-for="(instance, index) in instances" :key="instance.key" class="content-panel artifact-instance-card">
          <div class="section-heading artifact-instance-heading">
            <h3 v-if="!instance.isNew">{{ instance.name }}</h3>
            <h3 v-else>{{ t('artifactManagerConfig.newInstance') }}</h3>
            <button v-if="editing && instance.name !== 'default'" class="text-button danger-link" type="button" @click="removeInstance(index)">{{ t('common.remove') }}</button>
          </div>
          <label v-if="editing && instance.isNew" class="field required-field"><span>{{ t('artifactManagerConfig.instanceName') }}</span><input v-model.trim="instance.name" required maxlength="253" autocomplete="off" :placeholder="t('artifactManagerConfig.namePlaceholder')" /></label>
          <div class="artifact-instance-fields">
            <label class="field required-field"><span>{{ t('artifactManagerConfig.internalURL') }}</span><input v-if="editing" v-model.trim="instance.internalURL" required type="url" autocomplete="off" placeholder="http://artifact-manager:8081" /><code v-else>{{ instance.internalURL }}</code></label>
            <label class="field"><span>{{ t('artifactManagerConfig.runnerInternalURL') }}</span><input v-if="editing" v-model.trim="instance.runnerInternalURL" type="url" autocomplete="off" :placeholder="t('artifactManagerConfig.optional')" /><code v-else>{{ instance.runnerInternalURL || t('common.emptyValue') }}</code></label>
            <label class="field"><span>{{ t('artifactManagerConfig.runnerExternalURL') }}</span><input v-if="editing" v-model.trim="instance.runnerExternalURL" type="url" autocomplete="off" placeholder="https://artifacts.example.com" /><code v-else>{{ instance.runnerExternalURL || t('common.emptyValue') }}</code></label>
          </div>
        </section>
        <div v-if="editing" class="artifact-config-actions">
          <button class="secondary-button" type="button" @click="cancelEdit">{{ t('common.cancel') }}</button>
          <button class="primary-button" type="submit">{{ saving ? t('common.saving') : t('common.save') }}</button>
        </div>
      </fieldset>
    </form>
  </section>
</template>

<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue';
import { onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { parse, stringify } from 'yaml';
import { ApiError, errorTranslationKey, request } from '@/api';
import type { Config } from '@/types';

type Instance = { internalURL: string; runnerInternalURL: string; runnerExternalURL: string };
type DraftInstance = Instance & { key: number; name: string; isNew: boolean };

const { t } = useI18n();
const path = '/apis/ebs/v1/configs/artifact-manager-conf';
const current = ref<Config | null>(null);
const instances = ref<DraftInstance[]>([]);
const loading = ref(false);
const saving = ref(false);
const editing = ref(false);
const error = ref('');
const success = ref(false);
let nextKey = 0;

onMounted(load);

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function readInstances(config: Config): DraftInstance[] {
  const content: unknown = parse(config.spec.content, { uniqueKeys: true });
  if (!isRecord(content) || !isRecord(content.instances) || !Object.hasOwn(content.instances, 'default')) throw new Error('artifactManagerConfig.invalidContent');
  if (Object.keys(content).some(key => key !== 'instances')) throw new Error('artifactManagerConfig.invalidContent');
  return Object.entries(content.instances).map(([name, value]) => {
    if (!isRecord(value) || Object.keys(value).some(key => !['internalURL', 'runnerInternalURL', 'runnerExternalURL'].includes(key)) ||
      typeof value.internalURL !== 'string' || (value.runnerInternalURL !== undefined && typeof value.runnerInternalURL !== 'string') ||
      (value.runnerExternalURL !== undefined && typeof value.runnerExternalURL !== 'string')) throw new Error('artifactManagerConfig.invalidContent');
    return { key: ++nextKey, name, isNew: false, internalURL: value.internalURL,
      runnerInternalURL: value.runnerInternalURL || '', runnerExternalURL: value.runnerExternalURL || '' };
  });
}

async function load(): Promise<void> {
  if (loading.value || saving.value || editing.value) return;
  loading.value = true;
  error.value = '';
  success.value = false;
  try {
    const config = await request<Config>(path);
    instances.value = readInstances(config);
    current.value = config;
  } catch (reason) {
    current.value = null;
    error.value = reason instanceof Error && reason.message === 'artifactManagerConfig.invalidContent'
      ? reason.message : errorTranslationKey(reason, 'artifactManagerConfig.loadFailed');
  } finally { loading.value = false; }
}

function startEdit(): void { editing.value = true; error.value = ''; success.value = false; }
function cancelEdit(): void {
  if (!current.value || saving.value) return;
  instances.value = readInstances(current.value);
  editing.value = false;
  error.value = '';
}
function addInstance(): void {
  instances.value.push({ key: ++nextKey, name: '', isNew: true, internalURL: '', runnerInternalURL: '', runnerExternalURL: '' });
  error.value = '';
}
function removeInstance(index: number): void {
  const instance = instances.value[index];
  if (!instance || instance.name === 'default' || (!instance.isNew && !window.confirm(t('artifactManagerConfig.removeWarning', { name: instance.name })))) return;
  instances.value.splice(index, 1);
  error.value = '';
}
function validURL(value: string, optional = false, httpsOnly = false): boolean {
  if (!value) return optional;
  try {
    const parsed = new URL(value);
    return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && (!httpsOnly || parsed.protocol === 'https:') &&
      Boolean(parsed.host) && !parsed.username && !parsed.password && !parsed.search && !parsed.hash;
  } catch { return false; }
}
function validName(value: string): boolean {
  return value.length <= 253 && /^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$/.test(value);
}
async function save(): Promise<void> {
  if (!current.value || !editing.value || saving.value) return;
  const names = new Set<string>();
  for (const instance of instances.value) {
    if (!validName(instance.name)) { error.value = 'artifactManagerConfig.invalidName'; return; }
    if (names.has(instance.name)) { error.value = 'artifactManagerConfig.duplicateName'; return; }
    names.add(instance.name);
    if (!validURL(instance.internalURL) || !validURL(instance.runnerInternalURL, true) || !validURL(instance.runnerExternalURL, true, true)) {
      error.value = 'artifactManagerConfig.invalidURL'; return;
    }
  }
  if (!names.has('default')) { error.value = 'artifactManagerConfig.invalidContent'; return; }
  const content = { instances: Object.fromEntries(instances.value.map(({ name, internalURL, runnerInternalURL, runnerExternalURL }) =>
    [name, { internalURL, runnerInternalURL, runnerExternalURL }])) };
  saving.value = true;
  error.value = '';
  try {
    const saved = await request<Config>(path, { method: 'PUT', body: JSON.stringify({ ...current.value,
      spec: { ...current.value.spec, visibility: 'OpsOnly', content: stringify(content) } }) });
    instances.value = readInstances(saved);
    current.value = saved;
    editing.value = false;
    success.value = true;
  } catch (reason) {
    error.value = reason instanceof ApiError && reason.status === 409 ? 'artifactManagerConfig.conflict'
      : errorTranslationKey(reason, 'artifactManagerConfig.saveFailed');
  } finally { saving.value = false; }
}
</script>

<style scoped>
.artifact-config-heading { align-items: center; }
.artifact-config-refresh { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.artifact-config-refresh :deep(svg) { width: 16px; height: 16px; display: block; }
.artifact-config-fields { min-width: 0; margin: 0; padding: 0; border: 0; }
.artifact-config-actions { display: flex; flex-wrap: wrap; gap: 10px; margin: 16px 0; }
.artifact-instance-card { padding: 16px 18px; }
.artifact-instance-card + .artifact-instance-card { margin-top: 12px; }
.artifact-instance-heading { margin-bottom: 14px; align-items: center; }
.artifact-instance-heading h3 { margin: 0; font-size: 16px; }
.artifact-instance-fields { display: grid; gap: 12px; }
.artifact-instance-fields .field { min-width: 0; }
.artifact-instance-fields code { display: block; min-width: 0; overflow-wrap: anywhere; }
</style>
