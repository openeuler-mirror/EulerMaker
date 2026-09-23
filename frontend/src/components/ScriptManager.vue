<template>
  <section class="content-panel">
    <div class="section-heading">
      <h2>{{ t('scripts.title') }}</h2>
      <button class="icon-button script-refresh" type="button" :title="t('common.refresh')" :aria-label="t('common.refresh')" :disabled="busy" @click="load"><Refresh /></button>
    </div>
    <p class="form-hint">{{ t('scripts.hint') }}</p>
    <p v-if="success" class="success-banner" role="status">{{ success }}</p>
    <div class="script-toolbar">
      <label class="field script-search"><span>{{ t('scripts.search') }}</span><input v-model="search" type="search" :placeholder="t('scripts.searchPlaceholder')" /></label>
      <button v-if="canManage" class="primary-button" type="button" :disabled="busy || editorOpen" @click="openCreate">{{ t('scripts.create') }}</button>
    </div>
    <p v-if="error" class="form-error" role="alert">{{ t(error) }}</p>
    <div v-if="loading" class="skeleton-list" :aria-label="t('scripts.loading')"><span v-for="item in 3" :key="item"></span></div>
    <template v-else-if="!error">
      <EmptyState v-if="!scripts.length" :title="t('scripts.empty')" :description="t('scripts.emptyHint')" />
      <EmptyState v-else-if="!filtered.length" :title="t('scripts.noMatches')" :description="t('scripts.searchPlaceholder')" />
      <div v-else class="project-table-wrap">
        <table class="project-table admin-table">
          <thead><tr><th>{{ t('scripts.name') }}</th><th>{{ t('scripts.size') }}</th><th>{{ t('admin.actions') }}</th></tr></thead>
          <tbody><tr v-for="script in filtered" :key="script.metadata?.uid || script.metadata?.name">
            <td><strong>{{ script.metadata?.name }}</strong></td><td>{{ contentSize(script) }} KiB</td>
            <td><button class="text-button" type="button" :disabled="busy || editorOpen" @click="openEdit(script)">{{ t(canManage ? 'common.edit' : 'scripts.view') }}</button></td>
          </tr></tbody>
        </table>
      </div>
    </template>
    <section v-if="editorOpen" class="script-editor" aria-labelledby="script-editor-title">
      <h3 id="script-editor-title">{{ t(!canManage ? 'scripts.view' : editing ? 'scripts.edit' : 'scripts.create') }}</h3>
      <form class="project-form" @submit.prevent="save">
        <label class="field required-field"><span>{{ t('scripts.name') }}</span><input v-model="name" required maxlength="253" autocomplete="off" :disabled="Boolean(editing) || saving || !canManage" :placeholder="t('scripts.namePlaceholder')" /></label>
        <label class="field required-field"><span>{{ t('scripts.content') }}</span><textarea v-model="content" class="yaml-editor script-content" rows="18" required spellcheck="false" autocapitalize="off" autocomplete="off" :readonly="!canManage" :disabled="saving"></textarea></label>
        <p class="form-hint">{{ t('scripts.contentHint') }}</p>
        <p v-if="dialogError" class="form-error" role="alert">{{ t(dialogError) }}</p>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="saving" @click="closeEditor">{{ t(canManage ? 'common.cancel' : 'common.close') }}</button><button v-if="canManage" class="primary-button" type="submit" :disabled="busy">{{ t(saving ? 'common.saving' : 'common.save') }}</button></div>
      </form>
    </section>
  </section>
</template>

<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { ApiError, errorTranslationKey, list, request } from '@/api';
import EmptyState from '@/components/EmptyState.vue';
import { useSessionStore } from '@/stores/session';
import type { Script } from '@/types';

const { t } = useI18n();
const session = useSessionStore();
const canManage = computed(() => session.role === 'ops' || session.role === 'admin');
const path = '/apis/ebs/v1/scripts';
const scripts = ref<Script[]>([]);
const search = ref('');
const filtered = computed(() => scripts.value.filter(script => (script.metadata?.name || '').toLowerCase().includes(search.value.trim().toLowerCase())));
const loading = ref(false);
const opening = ref(false);
const saving = ref(false);
const busy = computed(() => loading.value || opening.value || saving.value);
const error = ref('');
const success = ref('');
const editorOpen = ref(false);
const editing = ref<Script | null>(null);
const name = ref('');
const content = ref('');
const dialogError = ref('');

onMounted(load);
function contentSize(script: Script): string { return (new TextEncoder().encode(script.spec?.content || '').length / 1024).toFixed(1); }
async function load(): Promise<void> {
  if (busy.value) return;
  loading.value = true;
  error.value = '';
  try {
    const items: Script[] = [];
    const seen = new Set<string>();
    let token = '';
    do {
      const query = new URLSearchParams({ limit: '100' });
      if (token) query.set('continue', token);
      const page = await list<Script>(`${path}?${query}`);
      items.push(...page.items);
      token = page.next;
      if (token && seen.has(token)) throw new Error('repeated continuation token');
      seen.add(token);
    } while (token);
    scripts.value = items;
  } catch (reason) { error.value = errorTranslationKey(reason, 'scripts.loadFailed'); }
  finally { loading.value = false; }
}
function openCreate(): void {
  if (!canManage.value || busy.value || editorOpen.value) return;
  editing.value = null;
  name.value = '';
  content.value = '#!/bin/bash\nset -euo pipefail\n';
  dialogError.value = '';
  success.value = '';
  editorOpen.value = true;
}
async function openEdit(script: Script): Promise<void> {
  if (busy.value || editorOpen.value || !script.metadata?.name) return;
  opening.value = true;
  error.value = '';
  success.value = '';
  try {
    const current = await request<Script>(`${path}/${encodeURIComponent(script.metadata.name)}`);
    if (!current.metadata?.resourceVersion || !current.metadata.uid || current.metadata.uid !== script.metadata.uid) throw new ApiError(409, 'errors.conflict');
    editing.value = current;
    name.value = current.metadata.name || script.metadata.name;
    content.value = current.spec.content;
    dialogError.value = '';
    editorOpen.value = true;
  } catch (reason) { error.value = errorTranslationKey(reason, 'scripts.loadFailed'); }
  finally { opening.value = false; }
}
function closeEditor(): void { if (!saving.value) { editorOpen.value = false; editing.value = null; } }
async function save(): Promise<void> {
  if (!canManage.value || busy.value || !editorOpen.value) return;
  const scriptName = name.value.trim();
  if (!scriptName || scriptName.length > 253 || !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*$/.test(scriptName)) {
    dialogError.value = 'scripts.invalidName'; return;
  }
  const firstLine = content.value.split('\n', 1)[0];
  if (content.value.includes('\0') || firstLine.includes('\r') || !/^#![ \t]*\/[^\s]+/.test(firstLine)) {
    dialogError.value = 'scripts.invalidContent'; return;
  }
  saving.value = true;
  dialogError.value = '';
  const current = editing.value;
  const body: Script = current ? { ...current, spec: { content: content.value } }
    : { apiVersion: 'ebs/v1', kind: 'Script', metadata: { name: scriptName }, spec: { content: content.value } };
  let saved = false;
  try {
    await request<Script>(current ? `${path}/${encodeURIComponent(scriptName)}` : path, { method: current ? 'PUT' : 'POST', body: JSON.stringify(body) });
    success.value = t(current ? 'scripts.saved' : 'scripts.created', { name: scriptName });
    editorOpen.value = false;
    editing.value = null;
    saved = true;
  } catch (reason) {
    dialogError.value = reason instanceof ApiError && reason.status === 409 ? (current ? 'scripts.conflict' : 'scripts.alreadyExists')
      : reason instanceof ApiError && reason.status === 413 ? 'scripts.tooLarge' : errorTranslationKey(reason, 'scripts.saveFailed');
  } finally { saving.value = false; }
  if (saved) await load();
}
</script>

<style scoped>
.script-toolbar { display: flex; align-items: end; flex-wrap: wrap; gap: 16px; margin: 16px 0; }
.script-search { width: 320px; max-width: 100%; }
.script-refresh { width: 28px; height: 28px; padding: 0; border-radius: 6px; }
.script-refresh :deep(svg) { width: 16px; height: 16px; }
.script-editor { margin-top: 24px; padding-top: 20px; border-top: 1px solid var(--border-color, #e5e7eb); }
.script-content { tab-size: 4; white-space: pre; overflow: auto; }
@media (max-width: 540px) { .script-search { width: 100%; } }
</style>
