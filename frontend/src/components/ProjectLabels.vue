<template>
  <section class="content-panel project-labels">
    <div class="section-heading">
      <h2>{{ t('project.labels') }}</h2>
      <button v-if="canEdit && !editing" class="secondary-button compact-button" type="button" @click="startEditing">{{ t('common.edit') }}</button>
    </div>
    <form v-if="editing" class="project-form" @submit.prevent="save">
      <p class="form-hint">{{ t('project.labelsHint') }}</p>
      <div v-for="(row, index) in rows" :key="index" class="label-edit-row">
        <label class="field"><span>{{ t('operations.labelKey') }}</span><input v-model="row.key" :disabled="saving || lockedRows.has(row)" required /></label>
        <label class="field"><span>{{ t('operations.labelValue') }}</span><input v-model="row.value" :disabled="saving || lockedRows.has(row)" maxlength="63" /></label>
        <button class="text-button danger-link" type="button" :disabled="saving || lockedRows.has(row)" @click="rows.splice(index, 1)">{{ t('common.remove') }}</button>
      </div>
      <button class="secondary-button" type="button" :disabled="saving" @click="rows.push({ key: '', value: '' })">{{ t('project.addLabel') }}</button>
      <div v-if="errorKey" class="form-error" role="alert">{{ t(errorKey) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="saving" @click="editing = false">{{ t('common.cancel') }}</button>
        <button class="primary-button" type="submit" :disabled="saving">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
      </div>
    </form>
    <dl v-else-if="entries.length" class="label-list">
      <div v-for="[key, value] in entries" :key="key" class="label-line"><dt>{{ key }}</dt><dd>{{ value || t('common.emptyValue') }}</dd></div>
    </dl>
    <p v-else class="form-hint">{{ t('project.noLabels') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { request, errorTranslationKey } from "@/api";
import { useSessionStore } from "@/stores/session";
import { PROJECT_OWNER_LABEL, projectLabelReadOnly, projectLabelPatch, type ProjectLabelRow } from "@/utils/projectLabels";
import type { Project } from "@/types";

const props = defineProps<{ project: Project }>();
const emit = defineEmits<{ saved: [project: Project] }>();
const { t } = useI18n();
const session = useSessionStore();
const canEdit = computed(() => {
  const metadata = props.project.metadata;
  const name = session.session?.identity?.name;
  return ["user", "ops", "admin"].includes(session.role) && !!name &&
    metadata?.labels?.[PROJECT_OWNER_LABEL] === name && !!metadata.uid && !!metadata.resourceVersion && !metadata.deletionTimestamp;
});
const entries = computed(() => Object.entries(props.project.metadata?.labels || {}).sort(([a], [b]) => a.localeCompare(b)));
const editing = ref(false);
const saving = ref(false);
const errorKey = ref("");
const rows = ref<ProjectLabelRow[]>([]);
const lockedRows = ref(new Set<ProjectLabelRow>());
let original: Record<string, string> = {};
let selected: Project["metadata"];
let pending: AbortController | undefined;

function startEditing(): void {
  if (!canEdit.value || saving.value) return;
  selected = { ...props.project.metadata };
  original = { ...selected.labels };
  rows.value = entries.value.map(([key, value]) => ({ key, value }));
  lockedRows.value = new Set(rows.value.filter(row => projectLabelReadOnly(row.key, session.role, original)));
  errorKey.value = "";
  editing.value = true;
}

async function save(): Promise<void> {
  if (!canEdit.value || saving.value || !editing.value || !selected?.name || !selected.uid) return;
  errorKey.value = "";
  const draft = projectLabelPatch(original, rows.value, original, session.role);
  if (draft.error) { errorKey.value = draft.error; return; }
  if (!Object.keys(draft.labels).length) { editing.value = false; return; }
  saving.value = true;
  const controller = new AbortController();
  pending = controller;
  try {
    const path = `/apis/ebs/v1/projects/${encodeURIComponent(selected.name)}`;
    const current = await request<Project>(path, { signal: controller.signal });
    if (controller.signal.aborted) return;
    if (current.metadata?.uid !== selected.uid || !current.metadata.resourceVersion || current.metadata.deletionTimestamp ||
      current.metadata.labels?.[PROJECT_OWNER_LABEL] !== session.session?.identity?.name) {
      errorKey.value = "errors.conflict";
      return;
    }
    const patch = projectLabelPatch(original, rows.value, current.metadata.labels || {}, session.role);
    if (patch.error) { errorKey.value = patch.error; return; }
    const updated = await request<Project>(path, {
      method: "PATCH", signal: controller.signal,
      headers: { "Content-Type": "application/merge-patch+json" },
      body: JSON.stringify({ metadata: { uid: current.metadata.uid, resourceVersion: current.metadata.resourceVersion, labels: patch.labels } }),
    });
    if (!controller.signal.aborted) { editing.value = false; emit("saved", updated); }
  } catch (error) {
    if (!controller.signal.aborted) errorKey.value = errorTranslationKey(error, "project.labelsFailed");
  } finally { saving.value = false; }
}

onBeforeUnmount(() => pending?.abort());
</script>

<style scoped>
.section-heading h2 { margin: 0; }
.label-list { margin: 0; font-size: 13px; }
.label-line { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 12px; padding: 7px 0; border-bottom: 1px solid var(--line); }
.label-line:last-child { border-bottom: 0; }
.label-line dt { color: var(--muted); }
.label-line dd { margin: 0; }
.label-line dt, .label-line dd { overflow-wrap: anywhere; }
.label-edit-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) auto; gap: 12px; align-items: end; }
.label-edit-row input { min-width: 0; width: 100%; }
.label-edit-row .text-button { min-height: 40px; }
@media (max-width: 560px) {
  .label-edit-row { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); }
  .label-edit-row .text-button { grid-column: 2; justify-self: end; }
}
</style>
