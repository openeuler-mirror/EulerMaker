<template>
  <section class="content-panel runner-taints">
    <div class="taints-heading">
      <h2>{{ t('runnerDetail.taints') }}</h2>
      <button v-if="canEdit && !editing" class="secondary-button" type="button" @click="startEditing">{{ t('common.edit') }}</button>
    </div>
    <form v-if="editing" class="project-form" @submit.prevent="save">
      <p class="form-hint">{{ t('runnerDetail.taintsHint') }}</p>
      <div v-for="(row, index) in rows" :key="index" class="taint-edit-row">
        <label class="field"><span>{{ t('operations.labelKey') }}</span><input v-model="row.key" :disabled="saving" required /></label>
        <label class="field"><span>{{ t('operations.labelValue') }}</span><input v-model="row.value" :disabled="saving" maxlength="63" /></label>
        <label class="field taint-effect"><span>{{ t('runnerDetail.effect') }}</span><AppSelect v-model="row.effect" :options="effectOptions" :label="t('runnerDetail.effect')" :disabled="saving" /></label>
        <button class="text-button danger-link" type="button" :disabled="saving" @click="rows.splice(index, 1)">{{ t('common.remove') }}</button>
      </div>
      <button class="secondary-button" type="button" :disabled="saving" @click="rows.push({ key: '', value: '', effect: 'NoSchedule' })">{{ t('runnerDetail.addTaint') }}</button>
      <div v-if="errorKey" class="form-error" role="alert">{{ t(errorKey) }}</div>
      <div class="modal-actions">
        <button class="secondary-button" type="button" :disabled="saving" @click="editing = false">{{ t('common.cancel') }}</button>
        <button class="primary-button" type="submit" :disabled="saving">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
      </div>
    </form>
    <div v-else-if="props.runner.spec?.taints?.length" class="table-scroll"><table class="taints-table">
      <thead><tr><th>{{ t('operations.labelKey') }}</th><th>{{ t('operations.labelValue') }}</th><th>{{ t('runnerDetail.effect') }}</th></tr></thead>
      <tbody><tr v-for="(taint, index) in props.runner.spec.taints" :key="index"><td>{{ taint.key }}</td><td>{{ taint.value || t('common.emptyValue') }}</td><td>{{ taint.effect }}</td></tr></tbody>
    </table></div>
    <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { request, errorTranslationKey } from "@/api";
import AppSelect from "@/components/AppSelect.vue";
import { useSessionStore } from "@/stores/session";
import { validRunnerLabelKey } from "@/utils/runnerLabels";
import type { Runner } from "@/types";

type Taint = NonNullable<NonNullable<Runner["spec"]>["taints"]>[number];
const props = defineProps<{ runner: Runner }>();
const emit = defineEmits<{ saved: [] }>();
const { t } = useI18n();
const session = useSessionStore();
const canEdit = computed(() => ["ops", "admin"].includes(session.role) && !!props.runner.metadata?.uid && !props.runner.metadata?.deletionTimestamp);
const effectOptions = ["NoSchedule", "PreferNoSchedule", "NoExecute"].map(value => ({ value, label: value }));
const editing = ref(false);
const saving = ref(false);
const errorKey = ref("");
const rows = ref<Taint[]>([]);
let original: Taint[] = [];
let selected: Runner["metadata"];
let pending: AbortController | undefined;

function startEditing(): void {
  if (!canEdit.value || saving.value) return;
  selected = { ...props.runner.metadata };
  original = (props.runner.spec?.taints || []).map(taint => ({ ...taint }));
  rows.value = original.map(taint => ({ ...taint, value: taint.value || "" }));
  errorKey.value = "";
  editing.value = true;
}

function equalTaints(a: Taint[], b: Taint[]): boolean {
  const normalize = (items: Taint[]) => items.map(({ key, value, effect }) => JSON.stringify([key, value || "", effect])).sort();
  return JSON.stringify(normalize(a)) === JSON.stringify(normalize(b));
}

async function save(): Promise<void> {
  if (!canEdit.value || saving.value || !editing.value || !selected?.name || !selected.uid) return;
  errorKey.value = "";
  const draft = rows.value.map(({ key, value, effect }) => ({ key, value: value || "", effect }));
  const keys = new Set<string>();
  for (const { key, value, effect } of draft) {
    const identity = JSON.stringify([key, effect]);
    if (!validRunnerLabelKey(key) || value.length > 63 || (value && !/^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$/.test(value)) || !effectOptions.some(option => option.value === effect) || keys.has(identity)) {
      errorKey.value = "runnerDetail.invalidTaints";
      return;
    }
    keys.add(identity);
  }
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
    const currentTaints = current.spec?.taints || [];
    if (!equalTaints(draft, original) && !equalTaints(draft, currentTaints)) {
      // Merge Patch replaces arrays, so never overwrite concurrent taint edits.
      if (!equalTaints(original, currentTaints)) { errorKey.value = "errors.conflict"; return; }
      await request<Runner>(path, {
        method: "PATCH", signal: controller.signal,
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({ metadata: { uid: current.metadata.uid, resourceVersion: current.metadata.resourceVersion }, spec: { taints: draft } }),
      });
    }
    if (!controller.signal.aborted) { editing.value = false; emit("saved"); }
  } catch (error) {
    if (!controller.signal.aborted) errorKey.value = errorTranslationKey(error, "runnerDetail.saveTaintsFailed");
  } finally { saving.value = false; }
}

onBeforeUnmount(() => pending?.abort());
</script>

<style scoped>
.taints-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
.taints-heading h2 { margin: 0; font-size: 16px; }
.taints-heading .secondary-button { min-height: 28px; padding: 4px 10px; font-size: 12px; }
.runner-taints .project-form { gap: 12px; }
.taint-edit-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 10px; align-items: end; padding-bottom: 12px; border-bottom: 1px solid var(--line); }
.taint-edit-row input { min-width: 0; width: 100%; }
.taint-effect { grid-column: 1 / -1; }
.taint-edit-row .text-button { grid-column: 2; justify-self: end; }
.taints-table { width: 100%; table-layout: fixed; border-collapse: collapse; text-align: left; }
.taints-table th, .taints-table td { padding: 6px; border-bottom: 1px solid var(--line); vertical-align: top; overflow-wrap: anywhere; font-size: 13px; }
.taints-table th { color: var(--muted); font-weight: 500; }
.taints-table tbody tr:last-child td { border-bottom: 0; }
</style>
