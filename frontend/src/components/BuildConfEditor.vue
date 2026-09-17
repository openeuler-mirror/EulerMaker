<template>
  <section class="content-panel">
    <div class="section-heading buildconf-heading"><h2>{{ t('buildConf.title') }}</h2><button class="icon-button buildconf-refresh-button" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="saving" @click="load"><Refresh /></button></div>
    <p class="form-hint">{{ t('buildConf.hint') }}</p>
    <p v-if="error" class="form-error" role="alert">{{ t(error) }}</p>
    <p v-if="success" class="success-banner" role="status">{{ t('buildConf.saved') }}</p>
    <form v-if="current" class="project-form" @submit.prevent="save">
      <fieldset :disabled="saving">
        <nav class="project-tabs"><button v-for="value in modes" :key="value" type="button" :class="{active: mode === value}" @click="switchMode(value)">{{ value === 'list' ? t('buildConf.list') : value.toUpperCase() }}</button></nav>
        <template v-if="mode === 'list'">
          <div v-for="(target, os) in draft.targets" :key="os" class="content-panel">
            <div class="section-heading"><h3>{{ os }}</h3><button type="button" class="text-button danger-link" @click="removeOS(os)">{{ t('common.remove') }}</button></div>
            <div v-for="(config, arch) in target.arches" :key="arch" class="form-grid">
              <label class="field"><span>{{ arch }}</span><input v-model.trim="config.image" required :placeholder="t('buildConf.image')" /></label><button type="button" class="text-button danger-link" @click="removeArch(os, arch)">{{ t('common.remove') }}</button>
            </div>
          </div>
          <div class="form-grid"><label class="field"><span>OS</span><input v-model.trim="newOS" /></label><label class="field"><span>Arch</span><input v-model.trim="newArch" /></label></div>
          <button class="secondary-button" type="button" @click="add">{{ t('buildConf.add') }}</button>
        </template>
        <label v-else class="field"><span>spec</span><textarea v-model="source" class="yaml-editor" rows="16" spellcheck="false"></textarea></label>
      </fieldset>
      <div class="modal-actions"><button class="primary-button" :disabled="saving" type="submit">{{ saving ? t('common.saving') : t('common.save') }}</button></div>
    </form>
  </section>
</template>
<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue';
import { onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { parse, stringify } from 'yaml';
import { ApiError, errorTranslationKey, request } from '@/api';
import type { BuildConf } from '@/types';
import { buildConfSpec, supportsTarget } from '@/components/buildConfDraft';
const { t } = useI18n();
const current = ref<BuildConf | null>(null);
const draft = ref<BuildConf['spec']>({targets:{}});
const modes = ['list', 'yaml', 'json'] as const;
const mode = ref<typeof modes[number]>('list');
const source = ref('');
const newOS = ref(''); const newArch = ref('');
const error = ref(''); const success = ref(false); const saving = ref(false);
const path = '/apis/ebs/v1/buildconfs/default';
onMounted(load);
async function load() {
  if (saving.value) return;
  saving.value = true; error.value = ''; success.value = false;
  try { current.value = await request<BuildConf>(path); draft.value = JSON.parse(JSON.stringify(current.value.spec)); mode.value = 'list'; }
  catch (reason) { current.value = null; error.value = errorTranslationKey(reason, 'buildConf.loadFailed'); }
  finally { saving.value = false; }
}
function readDraft(): BuildConf['spec'] {
  const value = mode.value === 'list' ? draft.value : mode.value === 'json' ? JSON.parse(source.value) : parse(source.value, { uniqueKeys: true });
  return buildConfSpec(value);
}
function switchMode(next: typeof modes[number]) {
  if (mode.value === next) return;
  try { const value = readDraft(); draft.value = value; source.value = next === 'json' ? JSON.stringify(value, null, 2) : stringify(value); mode.value = next; error.value = ''; }
  catch { error.value = 'buildConf.invalid'; }
}
function add() {
  if (!newOS.value || !newArch.value || ['__proto__','constructor','prototype'].includes(newOS.value) || ['__proto__','constructor','prototype'].includes(newArch.value)) { error.value = 'buildConf.invalid'; return; }
  const targets = draft.value.targets;
  if (Object.hasOwn(targets, newOS.value) && Object.hasOwn(targets[newOS.value].arches, newArch.value)) { error.value = 'buildConf.duplicate'; return; }
  if (!Object.hasOwn(targets, newOS.value)) targets[newOS.value] = { arches: {} };
  targets[newOS.value].arches[newArch.value] = { image: '' }; newArch.value = ''; error.value = '';
}
function removeOS(os: string) { if (window.confirm(t('buildConf.removeWarning'))) delete draft.value.targets[os]; }
function removeArch(os: string, arch: string) {
  if (!window.confirm(t('buildConf.removeWarning'))) return;
  delete draft.value.targets[os].arches[arch];
  if (!Object.keys(draft.value.targets[os].arches).length) delete draft.value.targets[os];
}
async function save() {
  if (!current.value || saving.value) return;
  let spec: BuildConf['spec'];
  try { spec = readDraft(); } catch { error.value = 'buildConf.invalid'; return; }
  const removed = Object.entries(current.value.spec.targets).some(([os, target]) => Object.keys(target.arches).some(arch => !supportsTarget({ spec }, { os, arch })));
  if (removed && !window.confirm(t('buildConf.removeWarning'))) return;
  saving.value = true; error.value = ''; success.value = false;
  try { current.value = await request<BuildConf>(path, {method:'PUT', body: JSON.stringify({...current.value, spec})}); draft.value = JSON.parse(JSON.stringify(current.value.spec)); mode.value = 'list'; success.value = true; }
  catch (reason) { error.value = reason instanceof ApiError && reason.status === 409 ? 'buildConf.conflict' : errorTranslationKey(reason, 'errors.requestFailed'); }
  finally { saving.value = false; }
}
</script>

<style scoped>
.buildconf-heading { align-items: center; }
.buildconf-refresh-button { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.buildconf-refresh-button :deep(svg) { width: 16px; height: 16px; display: block; }
</style>
