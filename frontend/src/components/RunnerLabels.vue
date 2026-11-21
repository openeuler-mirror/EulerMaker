<template>
  <section class="content-panel runner-labels">
    <div class="labels-heading">
      <h2>{{ t('runnerDetail.labels') }}</h2>
      <button v-if="canEdit && !editing" class="secondary-button" type="button" @click="startEditing">{{ t('common.edit') }}</button>
    </div>
    <form v-if="editing" class="project-form" @submit.prevent="save">
      <p class="form-hint">{{ t('operations.runnerLabelsHint') }}</p>
      <div v-for="(row, index) in rows" :key="index" class="label-edit-row">
        <label class="field"><span>{{ t('operations.labelKey') }}</span><input v-model="row.key" :disabled="saving || lockedRows.has(row)" required /></label>
        <label class="field"><span>{{ t('operations.labelValue') }}</span><input v-model="row.value" :disabled="saving || lockedRows.has(row)" maxlength="63" /></label>
        <button class="text-button danger-link" type="button" :disabled="saving || lockedRows.has(row)" @click="rows.splice(index, 1)">{{ t('common.remove') }}</button>
      </div>
      <button class="secondary-button" type="button" :disabled="saving" @click="rows.push({ key: '', value: '' })">{{ t('operations.addRunnerLabel') }}</button>
      <div v-if="errorKey" class="form-error" role="alert">{{ t(errorKey) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="saving" @click="editing = false">{{ t('common.cancel') }}</button>
        <button class="primary-button" type="submit" :disabled="saving">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
      </div>
    </form>
    <dl v-else-if="entries.length" class="label-list">
      <div v-for="[key, value] in entries" :key="key" class="label-line"><dt>{{ key }}</dt><dd>{{ value || t('common.emptyValue') }}</dd></div>
    </dl>
    <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { request, errorTranslationKey } from "@/api";
import { useSessionStore } from "@/stores/session";
import { isSystemRunnerLabel, runnerLabelPatch, runnerLabelRows, type RunnerLabelRow } from "@/utils/runnerLabels";
import type { Runner } from "@/types";

const props = defineProps<{ runner: Runner }>();
const emit = defineEmits<{ saved: [] }>();
const { t } = useI18n();
const session = useSessionStore();
const canEdit = computed(() => ["ops", "admin"].includes(session.role) && !!props.runner.metadata?.uid && !props.runner.metadata?.deletionTimestamp);
const entries = computed(() => Object.entries(props.runner.metadata?.labels || {}).sort(([a], [b]) => a.localeCompare(b)));
const editing = ref(false);
const saving = ref(false);
const errorKey = ref("");
const rows = ref<RunnerLabelRow[]>([]);
const lockedRows = ref(new Set<RunnerLabelRow>());
let original: Record<string, string> = {};
let selected: Runner["metadata"];
let pending: AbortController | undefined;

function startEditing(): void {
  if (!canEdit.value || saving.value) return;
  selected = { ...props.runner.metadata };
  original = { ...selected.labels };
  rows.value = runnerLabelRows(original);
  lockedRows.value = new Set(rows.value.filter(row => isSystemRunnerLabel(row.key)));
  errorKey.value = "";
  editing.value = true;
}

async function save(): Promise<void> {
  if (!canEdit.value || saving.value || !editing.value || !selected?.name || !selected.uid) return;
  errorKey.value = "";
  const draft = runnerLabelPatch(original, rows.value, original);
  if (draft.error) { errorKey.value = draft.error; return; }
  saving.value = true;
  const controller = new AbortController();
  pending = controller;
  try {
    const path = `/apis/ebs/v1/runners/${encodeURIComponent(selected.name)}`;
    const current = await request<Runner>(path, { signal: controller.signal });
    if (controller.signal.aborted) return;
    if (current.metadata?.uid !== selected.uid || !current.metadata.resourceVersion || current.metadata.deletionTimestamp) {
      errorKey.value = "errors.conflict";
      return;
    }
    const patch = runnerLabelPatch(original, rows.value, current.metadata.labels || {});
    if (patch.error) { errorKey.value = patch.error; return; }
    if (Object.keys(patch.labels).length) {
      await request<Runner>(path, {
        method: "PATCH", signal: controller.signal,
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({ metadata: { uid: current.metadata.uid, resourceVersion: current.metadata.resourceVersion, labels: patch.labels } }),
      });
    }
    if (!controller.signal.aborted) { editing.value = false; emit("saved"); }
  } catch (error) {
    if (!controller.signal.aborted) errorKey.value = errorTranslationKey(error, "operations.runnerLabelsFailed");
  } finally { saving.value = false; }
}

onBeforeUnmount(() => pending?.abort());
</script>

<style scoped>
.labels-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
.labels-heading h2 { margin: 0; font-size: 16px; }
.labels-heading .secondary-button { min-height: 28px; padding: 4px 10px; font-size: 12px; }
.label-list { margin: 0; font-size: 13px; }
.label-line { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 12px; padding: 7px 0; border-bottom: 1px solid var(--line); }
.runner-labels .project-form { gap: 12px; }
.label-line:first-child { padding-top: 0; }
.label-line:last-child { border-bottom: 0; padding-bottom: 0; }
.label-line dt { color: var(--muted); }
.label-line dd { margin: 0; }
.label-line dt, .label-line dd { overflow-wrap: anywhere; }
.label-edit-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) auto; gap: 12px; align-items: end; }
.label-edit-row input { min-width: 0; width: 100%; }
.label-edit-row .text-button { min-height: 40px; }
@media (max-width: 560px) {
  .label-line { gap: 12px; }
  .label-edit-row { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); }
  .label-edit-row .text-button { grid-column: 2; justify-self: end; }
}
</style>
