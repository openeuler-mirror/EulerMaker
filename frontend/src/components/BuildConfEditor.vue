<template>
  <section class="content-panel">
    <div class="section-heading buildconf-heading"><h2>{{ t('buildConf.title') }}</h2><button class="icon-button buildconf-refresh-button" type="button" :aria-label="t('common.refresh')" :title="t('common.refresh')" :disabled="saving" @click="load"><Refresh /></button></div>
    <p class="form-hint">{{ t('buildConf.hint') }}</p>
    <p v-if="error" class="form-error" role="alert">{{ t(error) }}</p>
    <p v-if="success" class="success-banner" role="status">{{ t('buildConf.saved') }}</p>
    <form v-if="current" class="project-form" @submit.prevent="save">
      <fieldset class="buildconf-fields" :disabled="saving">
        <nav class="project-tabs"><button v-for="value in modes" :key="value" type="button" :class="{active: mode === value}" @click="switchMode(value)">{{ value === 'list' ? t('buildConf.list') : value.toUpperCase() }}</button></nav>
        <div class="buildconf-actions">
          <button v-if="editing && mode === 'list'" class="secondary-button" type="button" @click="openAddDialog">{{ t('buildConf.add') }}</button>
          <button v-if="!editing" class="secondary-button" type="button" @click="startEdit">{{ t('common.edit') }}</button>
          <template v-else>
            <button class="secondary-button" type="button" @click="cancelEdit">{{ t('common.cancel') }}</button>
            <button class="primary-button" type="submit">{{ saving ? t('common.saving') : t('common.save') }}</button>
          </template>
        </div>
        <template v-if="mode === 'list'">
          <p v-if="!targetGroups.length" class="form-hint">{{ t('buildConf.empty') }}</p>
          <div v-for="group in targetGroups" :key="group.os" class="content-panel buildconf-os-card">
            <div class="section-heading buildconf-os-heading"><h3>{{ group.os }}</h3><button v-if="editing" type="button" class="text-button danger-link" @click="removeOS(group.os)">{{ t('common.remove') }}</button></div>
            <div v-for="entry in group.entries" :key="entry.arch" class="buildconf-arch-row" :class="{ 'is-readonly': !editing }">
              <strong>{{ entry.arch }}</strong>
              <label v-if="editing" class="field buildconf-image-field"><input v-model.trim="entry.config.image" required :aria-label="t('buildConf.imageForTarget', { os: group.os, arch: entry.arch })" :placeholder="t('buildConf.image')" /></label>
              <code v-else class="buildconf-image-value">{{ entry.config.image || t('common.emptyValue') }}</code>
              <button v-if="editing" type="button" class="text-button danger-link" @click="removeArch(group.os, entry.arch)">{{ t('common.remove') }}</button>
            </div>
          </div>
        </template>
        <label v-else class="field"><span>spec</span><textarea v-model="source" class="yaml-editor" rows="16" spellcheck="false" :readonly="!editing"></textarea></label>
      </fieldset>
    </form>
  </section>
  <ModalDialog v-if="addDialogOpen" title-id="buildconf-add-target-title" :title="t('buildConf.add')" :close-label="t('common.close')" @close="closeAddDialog">
    <form class="project-form" @submit.prevent="add">
      <p class="form-hint">{{ t('buildConf.addHint') }}</p>
      <div class="form-grid">
        <div class="field required-field buildconf-os-field"><span id="buildconf-os-label">{{ t('buildConf.os') }}</span>
          <div ref="osSuggestRoot" class="buildconf-os-combobox" @focusout="onOSFocusOut">
            <input v-model.trim="newOS" required autocomplete="off" role="combobox" aria-autocomplete="list" aria-labelledby="buildconf-os-label" aria-controls="buildconf-os-options" :aria-expanded="osSuggestionsOpen && matchingOS.length > 0" :aria-activedescendant="osSuggestionsOpen && activeOSIndex >= 0 ? `buildconf-os-option-${activeOSIndex}` : undefined" @focus="osSuggestionsOpen = true" @input="onOSInput" @keydown="onOSKeydown" />
            <div v-if="osSuggestionsOpen && matchingOS.length" id="buildconf-os-options" ref="osSuggestList" class="buildconf-os-options" role="listbox" :aria-label="t('buildConf.os')">
              <button v-for="(os, index) in matchingOS" :id="`buildconf-os-option-${index}`" :key="os" type="button" role="option" tabindex="-1" :aria-selected="activeOSIndex === index" :class="{ active: activeOSIndex === index }" @mousedown.prevent @mouseenter="activeOSIndex = index" @click="selectOS(os)">{{ os }}</button>
            </div>
          </div>
        </div>
        <label class="field required-field"><span>{{ t('buildConf.arch') }}</span><input v-model.trim="newArch" required autocomplete="off" /></label>
      </div>
      <label class="field required-field"><span>{{ t('buildConf.image') }}</span><input v-model.trim="newImage" required autocomplete="off" /></label>
      <div v-if="addError" class="form-error" role="alert">{{ t(addError) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" @click="closeAddDialog">{{ t('common.cancel') }}</button><button class="primary-button" type="submit">{{ t('buildConf.add') }}</button></div>
    </form>
  </ModalDialog>
</template>
<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue';
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { parse, stringify } from 'yaml';
import { ApiError, errorTranslationKey, request } from '@/api';
import ModalDialog from '@/components/ModalDialog.vue';
import type { BuildConf } from '@/types';
import { buildConfSpec, supportsTarget } from '@/components/buildConfDraft';
const { t } = useI18n();
const current = ref<BuildConf | null>(null);
const draft = ref<BuildConf['spec']>({targets:{}});
const modes = ['list', 'yaml', 'json'] as const;
const mode = ref<typeof modes[number]>('list');
const source = ref('');
const newOS = ref(''); const newArch = ref(''); const newImage = ref('');
const addDialogOpen = ref(false); const addError = ref('');
const osSuggestRoot = ref<HTMLElement | null>(null);
const osSuggestList = ref<HTMLElement | null>(null);
const osSuggestionsOpen = ref(false);
const activeOSIndex = ref(-1);
const error = ref(''); const success = ref(false); const saving = ref(false);
const editing = ref(false);
const targetGroups = computed(() => Object.entries(draft.value.targets).map(([os, target]) => ({
  os,
  entries: Object.entries(target.arches).map(([arch, config]) => ({ arch, config })),
})));
const targetEntries = computed(() => targetGroups.value.flatMap(group => group.entries));
const matchingOS = computed(() => targetGroups.value.map(group => group.os).filter(os => os.toLocaleLowerCase().includes(newOS.value.toLocaleLowerCase())));
const path = '/apis/ebs/v1/buildconfs/default';
onMounted(load);
onMounted(() => document.addEventListener('pointerdown', onOSPointerDown));
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOSPointerDown));
async function load() {
  if (saving.value) return;
  saving.value = true; error.value = ''; success.value = false;
  try { current.value = await request<BuildConf>(path); draft.value = JSON.parse(JSON.stringify(current.value.spec)); mode.value = 'list'; editing.value = false; addDialogOpen.value = false; }
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
function startEdit() { editing.value = true; error.value = ''; success.value = false; }
function cancelEdit() {
  if (!current.value) return;
  draft.value = JSON.parse(JSON.stringify(current.value.spec));
  if (mode.value === 'json') source.value = JSON.stringify(current.value.spec, null, 2);
  if (mode.value === 'yaml') source.value = stringify(current.value.spec);
  editing.value = false; addDialogOpen.value = false; error.value = '';
}
function openAddDialog() {
  if (!editing.value) return;
  newOS.value = ''; newArch.value = ''; newImage.value = ''; addError.value = ''; osSuggestionsOpen.value = false; activeOSIndex.value = -1; addDialogOpen.value = true;
}
function closeAddDialog() { osSuggestionsOpen.value = false; addDialogOpen.value = false; }
function onOSInput() { activeOSIndex.value = -1; osSuggestionsOpen.value = true; }
function selectOS(os: string) { newOS.value = os; activeOSIndex.value = -1; osSuggestionsOpen.value = false; }
function onOSFocusOut(event: FocusEvent) {
  if (!osSuggestRoot.value?.contains(event.relatedTarget as Node | null)) osSuggestionsOpen.value = false;
}
function onOSPointerDown(event: PointerEvent) {
  if (!osSuggestRoot.value?.contains(event.target as Node)) osSuggestionsOpen.value = false;
}
function onOSKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape' && osSuggestionsOpen.value) {
    event.preventDefault(); event.stopPropagation(); osSuggestionsOpen.value = false; return;
  }
  if (event.key === 'Enter' && osSuggestionsOpen.value && activeOSIndex.value >= 0) {
    event.preventDefault(); selectOS(matchingOS.value[activeOSIndex.value]); return;
  }
  if ((event.key !== 'ArrowDown' && event.key !== 'ArrowUp') || !matchingOS.value.length) return;
  event.preventDefault(); osSuggestionsOpen.value = true;
  const step = event.key === 'ArrowDown' ? 1 : -1;
  activeOSIndex.value = activeOSIndex.value < 0 ? (step > 0 ? 0 : matchingOS.value.length - 1) : (activeOSIndex.value + step + matchingOS.value.length) % matchingOS.value.length;
  void nextTick(() => osSuggestList.value?.children[activeOSIndex.value]?.scrollIntoView({ block: 'nearest' }));
}
function add() {
  if (!editing.value || !addDialogOpen.value) return;
  if (!newOS.value || !newArch.value || !newImage.value || ['__proto__','constructor','prototype'].includes(newOS.value) || ['__proto__','constructor','prototype'].includes(newArch.value)) { addError.value = 'buildConf.invalid'; return; }
  const targets = draft.value.targets;
  if (Object.hasOwn(targets, newOS.value) && Object.hasOwn(targets[newOS.value].arches, newArch.value)) { addError.value = 'buildConf.duplicate'; return; }
  if (!Object.hasOwn(targets, newOS.value)) targets[newOS.value] = { arches: {} };
  targets[newOS.value].arches[newArch.value] = { image: newImage.value };
  error.value = ''; success.value = false; addDialogOpen.value = false;
}
function removeOS(os: string) { if (window.confirm(t('buildConf.removeWarning'))) delete draft.value.targets[os]; }
function removeArch(os: string, arch: string) {
  if (!window.confirm(t('buildConf.removeWarning'))) return;
  delete draft.value.targets[os].arches[arch];
  if (!Object.keys(draft.value.targets[os].arches).length) delete draft.value.targets[os];
}
async function save() {
  if (!current.value || saving.value || !editing.value) return;
  if (mode.value === 'list' && targetEntries.value.some(entry => !entry.config.image.trim())) { error.value = 'buildConf.invalid'; return; }
  let spec: BuildConf['spec'];
  try { spec = readDraft(); } catch { error.value = 'buildConf.invalid'; return; }
  const removed = Object.entries(current.value.spec.targets).some(([os, target]) => Object.keys(target.arches).some(arch => !supportsTarget({ spec }, { os, arch })));
  if (removed && !window.confirm(t('buildConf.removeWarning'))) return;
  saving.value = true; error.value = ''; success.value = false;
  try {
    current.value = await request<BuildConf>(path, {method:'PUT', body: JSON.stringify({...current.value, spec})});
    draft.value = JSON.parse(JSON.stringify(current.value.spec));
    if (mode.value === 'json') source.value = JSON.stringify(current.value.spec, null, 2);
    if (mode.value === 'yaml') source.value = stringify(current.value.spec);
    editing.value = false; success.value = true;
  }
  catch (reason) { error.value = reason instanceof ApiError && reason.status === 409 ? 'buildConf.conflict' : errorTranslationKey(reason, 'errors.requestFailed'); }
  finally { saving.value = false; }
}
</script>

<style scoped>
.buildconf-fields { min-width: 0; margin: 0; padding: 0; border: 0; }
.buildconf-fields > .project-tabs { margin-top: 8px; }
.buildconf-heading { align-items: center; }
.buildconf-refresh-button { width: 28px; height: 28px; padding: 0; flex: none; border-radius: 6px; }
.buildconf-refresh-button :deep(svg) { width: 16px; height: 16px; display: block; }
.buildconf-os-card { padding: 16px 18px; }
.buildconf-os-card + .buildconf-os-card { margin-top: 12px; }
.buildconf-os-heading { margin-bottom: 8px; align-items: center; }
.buildconf-os-heading h3 { margin: 0; font-size: 16px; }
.buildconf-arch-row { min-width: 0; padding: 7px 0; display: grid; grid-template-columns: minmax(80px, 140px) minmax(0, 1fr) auto; align-items: center; gap: 12px; border-bottom: 1px solid var(--line); }
.buildconf-arch-row.is-readonly { grid-template-columns: minmax(80px, 140px) minmax(0, 1fr); }
.buildconf-arch-row:last-child { border-bottom: 0; }
.buildconf-arch-row strong { min-width: 0; overflow-wrap: anywhere; font-size: 13px; }
.buildconf-image-field { min-width: 0; }
.buildconf-image-value { min-width: 0; overflow-wrap: anywhere; font-size: 13px; }
.buildconf-actions { margin: 16px 0; display: flex; justify-content: flex-start; flex-wrap: wrap; gap: 10px; }
.buildconf-os-combobox { position: relative; }
.buildconf-os-combobox input { width: 100%; }
.buildconf-os-options { position: absolute; top: calc(100% + 5px); right: 0; left: 0; z-index: 5; max-height: 220px; padding: 5px; overflow-y: auto; background: var(--surface); border: 1px solid var(--line); border-radius: 8px; box-shadow: 0 12px 28px rgb(25 54 96 / 14%); }
.buildconf-os-options button { width: 100%; min-height: 36px; padding: 7px 10px; display: block; border: 0; border-radius: 5px; color: #455269; background: transparent; text-align: left; font-size: 14px; overflow-wrap: anywhere; }
.buildconf-os-options button:hover, .buildconf-os-options button.active { color: var(--blue); background: #edf6ff; }
.yaml-editor[readonly] { background: #f7f9fc; }
@media (max-width: 640px) {
  .buildconf-arch-row { grid-template-columns: minmax(64px, 100px) minmax(0, 1fr) auto; gap: 8px; }
}
</style>
