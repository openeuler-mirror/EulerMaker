<template>
  <fieldset class="resource-spec-editor" :disabled="disabled">
    <div class="project-tabs resource-editor-modes" :aria-label="t('operations.editMode')">
      <button type="button" :class="{ active: mode === 'list' }" :aria-pressed="mode === 'list'" @click="switchMode('list')">{{ t('operations.listEditor') }}</button>
      <button type="button" :class="{ active: mode === 'json' }" :aria-pressed="mode === 'json'" @click="switchMode('json')">{{ t('operations.jsonEditor') }}</button>
    </div>
    <div v-if="error" class="form-error" role="alert">{{ t(error) }}</div>
    <template v-if="mode === 'json'">
      <label class="field required-field"><span>{{ t('operations.resourceSpec') }}</span>
        <textarea v-model="source" class="yaml-editor" spellcheck="false" required></textarea>
      </label>
      <p class="form-hint">{{ t('operations.specHint') }}</p>
    </template>
    <template v-else>
      <section class="resource-rule-card resource-default-card">
        <h3>{{ t('operations.tableDefaults') }}</h3>
        <ResourceRequirementsFields v-model="draft.defaults" />
      </section>
      <p class="form-hint">{{ t('operations.listEditorHint') }}</p>
      <div class="resource-rule-toolbar">
        <label class="field resource-package-search"><span>{{ t('operations.searchPackages') }}</span><input v-model="search" type="search" /></label>
        <button class="secondary-button" type="button" @click="addPackage">{{ t('operations.addPackageRule') }}</button>
      </div>
      <div class="resource-rule-list">
        <section v-for="pkg in visiblePackages" :key="rowKey(pkg)" class="resource-rule-card resource-package-card">
          <div class="resource-rule-toolbar">
            <label class="field required-field"><span>{{ t('operations.packageName') }}</span><input v-model="pkg.name" required /></label>
            <button class="text-button danger-link" type="button" @click="draft.packages.splice(draft.packages.indexOf(pkg), 1)">{{ t('common.remove') }}</button>
          </div>
          <h4>{{ t('operations.packageDefaults') }}</h4>
          <ResourceRequirementsFields v-model="pkg.defaults" />
          <section v-for="arch in pkg.arches" :key="rowKey(arch)" class="resource-arch-rule">
            <div class="resource-rule-toolbar">
              <label class="field required-field"><span>{{ t('operations.arch') }}</span><input v-model="arch.name" required placeholder="x86_64 / aarch64" /></label>
              <button class="text-button danger-link" type="button" @click="pkg.arches.splice(pkg.arches.indexOf(arch), 1)">{{ t('common.remove') }}</button>
            </div>
            <ResourceRequirementsFields v-model="arch.resources" />
          </section>
          <button class="text-button" type="button" @click="pkg.arches.push({ name: '', resources: {} })">{{ t('operations.addArchRule') }}</button>
        </section>
      </div>
      <p v-if="!visiblePackages.length" class="form-hint">{{ t('operations.noMatchingRules') }}</p>
      <button v-if="filteredPackages.length > visiblePackages.length" class="secondary-button" type="button" @click="visibleLimit += 50">{{ t('operations.moreRules') }}</button>
    </template>
  </fieldset>
</template>
<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import ResourceRequirementsFields from "./ResourceRequirementsFields.vue";
import { parseResourceDraft, resourceSpec, type ResourceDraft } from "./buildResourceDraft";
const props = defineProps<{ source: string; disabled?: boolean }>();
const { t } = useI18n();
const source = ref(props.source);
const mode = ref<"list" | "json">("list");
const error = ref("");
const search = ref("");
const draft = ref<ResourceDraft>({ extra: {}, defaults: {}, packages: [] });
try { draft.value = parseResourceDraft(source.value); }
catch { mode.value = "json"; }
const visibleLimit = ref(50);
const filteredPackages = computed(() => draft.value.packages.filter(pkg => pkg.name.toLowerCase().includes(search.value.toLowerCase())));
const visiblePackages = computed(() => filteredPackages.value.slice(0, visibleLimit.value));
watch(search, () => { visibleLimit.value = 50; });
const keys = new WeakMap<object, number>();
let sequence = 0;
function rowKey(row: object): number {
  if (!keys.has(row)) keys.set(row, ++sequence);
  return keys.get(row)!;
}
function addPackage(): void {
  search.value = "";
  draft.value.packages.unshift({ name: "", extra: {}, defaults: {}, arches: [] });
}
function switchMode(next: "list" | "json"): void {
  if (next === mode.value) return;
  error.value = "";
  try {
    if (next === "list") { draft.value = parseResourceDraft(source.value); search.value = ""; }
    else source.value = JSON.stringify(resourceSpec(draft.value), null, 2);
    mode.value = next;
  } catch (err) { error.value = err instanceof Error ? err.message : "operations.invalidSpec"; }
}
function getSpec(): Record<string, unknown> {
  error.value = "";
  try {
    if (mode.value === "list") return resourceSpec(draft.value);
    const value: unknown = JSON.parse(source.value);
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error();
    return value as Record<string, unknown>;
  } catch (err) {
    error.value = err instanceof Error && err.message.startsWith("operations.") ? err.message : "operations.invalidSpec";
    throw new Error(error.value);
  }
}
defineExpose({ getSpec });
</script>
<style scoped>
.resource-spec-editor { border: 0; padding: 0; margin: 0; min-width: 0; display: grid; gap: 16px; }
.resource-editor-modes { margin-top: 0; }
.resource-rule-card { border: 1px solid var(--line); border-radius: 8px; padding: 14px; display: grid; gap: 12px; }
.resource-default-card { background: #edf5ff; border-color: #c5daf5; }
.resource-package-card { background: #f7f8fa; }
.resource-rule-card h3, .resource-rule-card h4 { margin: 0; font-size: 14px; }
.resource-rule-toolbar { display: flex; gap: 12px; align-items: end; }
.resource-rule-toolbar .field { flex: 1; min-width: 0; }
.resource-rule-toolbar .resource-package-search { flex: 0 1 320px; max-width: 100%; }
.resource-rule-list { display: grid; gap: 14px; max-height: 48vh; overflow: auto; }
.resource-arch-rule { border-top: 1px solid var(--line); padding-top: 12px; display: grid; gap: 12px; }
:deep(.resource-quantity-grid) { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
@media (max-width: 540px) {
  :deep(.resource-quantity-grid) { grid-template-columns: 1fr; }
  .resource-rule-toolbar { flex-wrap: wrap; }
  .resource-rule-toolbar .resource-package-search { flex: 1 1 100%; }
}
</style>
