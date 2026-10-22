<template>
  <section class="content-panel">
    <div class="section-heading service-config-heading">
      <h2>{{ t('serviceConfig.title') }}</h2>
      <button class="icon-button service-config-refresh" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="loading || saving || editing" @click="load"><Refresh /></button>
    </div>
    <p class="form-hint">{{ t('serviceConfig.hint') }}</p>
    <p v-if="error" class="form-error" role="alert">{{ t(error) }}</p>
    <p v-if="success" class="success-banner" role="status">{{ t('serviceConfig.saved') }}</p>
    <div v-if="loading" class="skeleton-list" :aria-label="t('serviceConfig.loading')"><span v-for="item in 3" :key="item"></span></div>
    <form v-else-if="current" class="project-form" @submit.prevent="save">
      <fieldset class="service-config-fields" :disabled="saving">
        <div class="service-config-actions">
          <button v-if="!editing" class="secondary-button" type="button" @click="startEdit">{{ t('common.edit') }}</button>
        </div>
        <section class="content-panel service-card">
          <div class="section-heading service-card-heading"><h3>{{ t('serviceConfig.gitServer') }}</h3></div>
          <div class="service-endpoint-fields">
            <label class="field required-field">
              <span>{{ t('serviceConfig.internalURL') }}</span>
              <input v-if="editing" v-model.trim="gitServer.internalURL" required type="url" autocomplete="off" placeholder="http://git-server:8080" />
              <code v-else>{{ gitServer.internalURL }}</code>
            </label>
            <label class="field">
              <span>{{ t('serviceConfig.runnerInternalURL') }}</span>
              <input v-if="editing" v-model.trim="gitServer.runnerInternalURL" type="url" autocomplete="off" :placeholder="t('serviceConfig.optional')" />
              <code v-else>{{ gitServer.runnerInternalURL || t('common.emptyValue') }}</code>
            </label>
            <label class="field">
              <span>{{ t('serviceConfig.runnerExternalURL') }}</span>
              <input v-if="editing" v-model.trim="gitServer.runnerExternalURL" type="url" autocomplete="off" placeholder="https://git.example.com" />
              <code v-else>{{ gitServer.runnerExternalURL || t('common.emptyValue') }}</code>
            </label>
          </div>
        </section>
        <section class="content-panel service-card">
          <div class="section-heading service-card-heading">
            <h3>{{ t('serviceConfig.artifactManagers') }}</h3>
            <button v-if="editing" class="secondary-button" type="button" @click="addInstance">{{ t('serviceConfig.addInstance') }}</button>
          </div>
          <section v-for="(instance, index) in instances" :key="instance.key" class="artifact-instance-card">
            <div class="section-heading artifact-instance-heading">
              <h4 v-if="!instance.isNew">{{ instance.name }}</h4>
              <h4 v-else>{{ t('serviceConfig.newInstance') }}</h4>
              <button v-if="editing" class="text-button danger-link" type="button" @click="removeInstance(index)">{{ t('common.remove') }}</button>
            </div>
            <label v-if="editing && instance.isNew" class="field required-field"><span>{{ t('serviceConfig.instanceName') }}</span><input v-model.trim="instance.name" required maxlength="63" autocomplete="off" :placeholder="t('serviceConfig.namePlaceholder')" /></label>
            <div class="service-endpoint-fields">
              <label class="field required-field"><span>{{ t('serviceConfig.internalURL') }}</span><input v-if="editing" v-model.trim="instance.internalURL" required type="url" autocomplete="off" placeholder="http://artifact-manager:8081" /><code v-else>{{ instance.internalURL }}</code></label>
              <label class="field"><span>{{ t('serviceConfig.runnerInternalURL') }}</span><input v-if="editing" v-model.trim="instance.runnerInternalURL" type="url" autocomplete="off" :placeholder="t('serviceConfig.optional')" /><code v-else>{{ instance.runnerInternalURL || t('common.emptyValue') }}</code></label>
              <label class="field"><span>{{ t('serviceConfig.runnerExternalURL') }}</span><input v-if="editing" v-model.trim="instance.runnerExternalURL" type="url" autocomplete="off" placeholder="https://artifacts.example.com" /><code v-else>{{ instance.runnerExternalURL || t('common.emptyValue') }}</code></label>
            </div>
          </section>
        </section>
        <div v-if="editing" class="service-config-actions">
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
import { buildServiceContent, parseServiceContent, type ServiceEndpoints, type ServiceConfigContent } from './serviceConfigDraft';

type DraftInstance = ServiceEndpoints & { key: number; name: string; isNew: boolean };

const { t } = useI18n();
const path = '/apis/ebs/v1/configs/service-config';
const current = ref<Config | null>(null);
const instances = ref<DraftInstance[]>([]);
const gitServer = ref<ServiceEndpoints>({ internalURL: '', runnerInternalURL: '', runnerExternalURL: '' });
const loading = ref(false);
const saving = ref(false);
const editing = ref(false);
const error = ref('');
const success = ref(false);
let nextKey = 0;

onMounted(load);

function readContent(config: Config): void {
  const content = parseServiceContent(parse(config.spec.content, { uniqueKeys: true }));
  instances.value = Object.entries(content.artifactManagers).map(([name, value]) =>
    ({ ...value, key: ++nextKey, name, isNew: false }));
  gitServer.value = { ...content.gitServer };
}

async function load(): Promise<void> {
  if (loading.value || saving.value || editing.value) return;
  loading.value = true;
  error.value = '';
  success.value = false;
  try {
    const config = await request<Config>(path);
    readContent(config);
    current.value = config;
  } catch (reason) {
    current.value = null;
    error.value = reason instanceof Error && reason.message === 'serviceConfig.invalidContent'
      ? reason.message : errorTranslationKey(reason, 'serviceConfig.loadFailed');
  } finally { loading.value = false; }
}

function startEdit(): void { editing.value = true; error.value = ''; success.value = false; }
function cancelEdit(): void {
  if (!current.value || saving.value) return;
  readContent(current.value);
  editing.value = false;
  error.value = '';
}
function addInstance(): void {
  instances.value.push({ key: ++nextKey, name: '', isNew: true, internalURL: '', runnerInternalURL: '', runnerExternalURL: '' });
  error.value = '';
}
function removeInstance(index: number): void {
  const instance = instances.value[index];
  if (!instance || (!instance.isNew && !window.confirm(t('serviceConfig.removeWarning', { name: instance.name })))) return;
  instances.value.splice(index, 1);
  error.value = '';
}
async function save(): Promise<void> {
  if (!current.value || !editing.value || saving.value) return;
  let content: ServiceConfigContent;
  try { content = buildServiceContent(instances.value, gitServer.value); }
  catch (reason) {
    error.value = reason instanceof Error ? reason.message : 'serviceConfig.invalidContent';
    return;
  }
  saving.value = true;
  error.value = '';
  try {
    const saved = await request<Config>(path, { method: 'PUT', body: JSON.stringify({ ...current.value,
      spec: { ...current.value.spec, visibility: 'OpsOnly', content: stringify(content) } }) });
    readContent(saved);
    current.value = saved;
    editing.value = false;
    success.value = true;
  } catch (reason) {
    error.value = reason instanceof ApiError && reason.status === 409 ? 'serviceConfig.conflict'
      : errorTranslationKey(reason, 'serviceConfig.saveFailed');
  } finally { saving.value = false; }
}
</script>

<style scoped>
.service-config-heading { align-items: center; }
.service-config-refresh { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.service-config-refresh :deep(svg) { width: 16px; height: 16px; display: block; }
.service-config-fields { min-width: 0; margin: 0; padding: 0; border: 0; }
.service-config-actions { display: flex; flex-wrap: wrap; gap: 10px; margin: 16px 0; }
.service-card { padding: 16px 18px; }
.service-card + .service-card { margin-top: 16px; }
.service-card-heading { margin-bottom: 16px; align-items: center; }
.service-card-heading h3 { margin: 0; font-size: 16px; }
.service-card code { display: block; min-width: 0; overflow-wrap: anywhere; }
.artifact-instance-card { padding: 16px; border: 1px solid var(--line); border-radius: 8px; }
.artifact-instance-card + .artifact-instance-card { margin-top: 12px; }
.artifact-instance-heading { margin-bottom: 14px; align-items: center; }
.artifact-instance-heading h4 { margin: 0; font-size: 14px; }
.artifact-instance-heading > .text-button { margin-top: 0; }
.service-endpoint-fields { display: grid; gap: 12px; }
.service-endpoint-fields .field { min-width: 0; }
.service-endpoint-fields code { display: block; min-width: 0; overflow-wrap: anywhere; }
</style>
