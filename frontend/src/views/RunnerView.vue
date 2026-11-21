<template>
  <div class="detail-toolbar runner-detail-toolbar">
    <RouterLink class="back-link" :to="{ name: 'operations', query: { section: 'runners' } }"><ArrowLeft />{{ t('runnerDetail.back') }}</RouterLink>
    <button class="icon-button" type="button" :disabled="loading" :aria-label="t('common.refresh')" :title="t('common.refresh')" @click="loadRunner"><Refresh /></button>
  </div>
  <section class="page-heading runner-detail-heading">
    <div><h1>{{ name }}</h1></div>
    <StatusBadge v-if="runner" :value="runner.metadata?.deletionTimestamp ? 'Deleting' : runner.status?.phase" />
  </section>
  <div v-if="loading" class="skeleton-list" :aria-label="t('operations.loadingRunners')"><span v-for="item in 4" :key="item"></span></div>
  <div v-else-if="errorKey" class="inline-error" role="alert"><WarningFilled />{{ t(errorKey) }}<button type="button" @click="loadRunner">{{ t('common.reload') }}</button></div>
  <div v-else-if="runner" class="runner-detail">
    <section class="content-panel">
      <h2>{{ t('runnerDetail.basic') }}</h2>
      <dl class="runner-basic-fields"><div v-for="field in fields" :key="field.label"><dt>{{ t(field.label) }}</dt><dd>{{ field.value }}</dd></div></dl>
    </section>
    <section class="content-panel">
      <h2>{{ t('runnerDetail.resources') }}</h2>
      <div class="resource-grid">
        <article v-for="key in resourceKeys" :key="key" class="resource-card">
          <h3>{{ resourceLabel(key) }}</h3>
          <dl class="resource-values">
            <div><dt>{{ t('runnerDetail.capacity') }}</dt><dd>{{ resourceValue(key, runner.status?.capacity?.[key]) }}</dd></div>
            <div><dt>{{ t('runnerDetail.allocatable') }}</dt><dd>{{ resourceValue(key, runner.status?.allocatable?.[key]) }}</dd></div>
          </dl>
        </article>
      </div>
    </section>
    <div class="runner-metadata-grid">
      <RunnerLabels :key="runner.metadata?.uid || name" :runner="runner" @saved="loadRunner" />
      <section v-for="group in metadataGroups" :key="group.title" class="content-panel">
        <h2>{{ t(group.title) }}</h2>
        <dl v-if="group.entries.length" class="runner-detail-fields metadata-fields"><div v-for="[key, value] in group.entries" :key="key"><dt>{{ key }}</dt><dd>{{ value || t('common.emptyValue') }}</dd></div></dl>
        <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
      </section>
      <section class="content-panel">
        <h2>{{ t('runnerDetail.taints') }}</h2>
        <div v-if="runner.spec?.taints?.length" class="table-scroll"><table class="taints-table">
          <thead><tr><th>{{ t('operations.labelKey') }}</th><th>{{ t('operations.labelValue') }}</th><th>{{ t('runnerDetail.effect') }}</th></tr></thead>
          <tbody><tr v-for="(taint, index) in runner.spec.taints" :key="index"><td>{{ taint.key }}</td><td>{{ taint.value || t('common.emptyValue') }}</td><td>{{ taint.effect }}</td></tr></tbody>
        </table></div>
        <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
      </section>
    </div>
    <section class="content-panel">
      <h2>{{ t('runnerDetail.addresses') }}</h2>
      <dl v-if="runner.status?.addresses?.length" class="runner-detail-fields"><div v-for="(address, index) in runner.status.addresses" :key="index"><dt>{{ address.type || t('common.emptyValue') }}</dt><dd>{{ address.address || t('common.emptyValue') }}</dd></div></dl>
      <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
    </section>
    <section class="content-panel">
      <h2>{{ t('runnerDetail.conditions') }}</h2>
      <div v-if="runner.status?.conditions?.length" class="table-scroll"><table class="data-table">
        <thead><tr><th>{{ t('operations.type') }}</th><th>{{ t('runnerDetail.conditionStatus') }}</th><th>{{ t('runnerDetail.reason') }}</th><th>{{ t('runnerDetail.message') }}</th><th>{{ t('runnerDetail.transitionTime') }}</th></tr></thead>
        <tbody><tr v-for="condition in runner.status.conditions" :key="condition.type"><td>{{ condition.type }}</td><td>{{ condition.status }}</td><td>{{ condition.reason || t('common.emptyValue') }}</td><td class="condition-message">{{ condition.message || t('common.emptyValue') }}</td><td>{{ formatDate(condition.lastTransitionTime) }}</td></tr></tbody>
      </table></div>
      <p v-else class="form-hint">{{ t('runnerDetail.none') }}</p>
    </section>
  </div>
</template>

<script setup lang="ts">
import { ArrowLeft, Refresh, WarningFilled } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";
import { useI18n } from "vue-i18n";
import { errorTranslationKey, request } from "@/api";
import StatusBadge from "@/components/StatusBadge.vue";
import RunnerLabels from "@/components/RunnerLabels.vue";
import type { Runner } from "@/types";
import { formatRunnerMemory } from "@/utils/runnerResources";

const route = useRoute();
const { t, locale } = useI18n();
const name = computed(() => String(route.params.name || ""));
const runner = ref<Runner | null>(null);
const loading = ref(false);
const errorKey = ref("");
let activeRequest: AbortController | undefined;

const fields = computed(() => {
  const item = runner.value;
  const text = (value?: string) => value || t("common.emptyValue");
  return [
    { label: "runnerDetail.instanceID", value: text(item?.spec?.instanceID) },
    { label: "operations.type", value: text(item?.spec?.type) },
    { label: "operations.arch", value: text(item?.spec?.arch) },
    { label: "operations.schedulable", value: t(item?.spec?.unschedulable || item?.status?.phase !== "Online" ? "common.no" : "common.yes") },
    { label: "operations.activeJobs", value: item?.status?.activeJobs ?? t("common.emptyValue") },
    { label: "runnerDetail.createdAt", value: formatDate(item?.metadata?.creationTimestamp) },
    { label: "operations.heartbeat", value: formatDate(item?.status?.heartbeat) },
    { label: "runnerDetail.os", value: text(item?.status?.info?.os) },
    { label: "runnerDetail.kernel", value: text(item?.status?.info?.kernelVersion) },
    { label: "runnerDetail.runtime", value: text(item?.status?.info?.runtimeVersion) },
    { label: "operations.agentVersion", value: text(item?.status?.info?.agentVersion) },
  ];
});
const resourceKeys = computed(() => [...new Set(["cpu", "memory", ...Object.keys(runner.value?.status?.capacity || {}), ...Object.keys(runner.value?.status?.allocatable || {})])]);
const metadataGroups = computed(() => [
  { title: "runnerDetail.annotations", entries: Object.entries(runner.value?.metadata?.annotations || {}).sort(([a], [b]) => a.localeCompare(b)) },
]);

function formatDate(value?: string): string {
  if (!value || Number.isNaN(Date.parse(value))) return t("common.emptyValue");
  return new Date(value).toLocaleString(locale.value, { hour12: false });
}
function resourceLabel(key: string): string {
  if (key === "cpu") return t("operations.runnerCPU");
  if (key === "memory") return t("operations.memory");
  return key === "ephemeral-storage" ? t("runnerDetail.ephemeralStorage") : key;
}
function resourceValue(key: string, value?: string): string {
  return (["memory", "ephemeral-storage"].includes(key) ? formatRunnerMemory(value) : value) || t("common.emptyValue");
}
async function loadRunner(): Promise<void> {
  activeRequest?.abort();
  const pending = new AbortController();
  activeRequest = pending;
  runner.value = null;
  loading.value = true;
  errorKey.value = "";
  try {
    const result = await request<Runner>(`/apis/ebs/v1/runners/${encodeURIComponent(name.value)}`, { signal: pending.signal });
    if (!pending.signal.aborted) runner.value = result;
  } catch (error) {
    if (!pending.signal.aborted) errorKey.value = errorTranslationKey(error, "operations.loadRunnersFailed");
  } finally {
    if (!pending.signal.aborted) loading.value = false;
  }
}
watch(name, () => { void loadRunner(); }, { immediate: true });
onBeforeUnmount(() => activeRequest?.abort());
</script>

<style scoped>
.runner-detail-toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px; }
.runner-detail-toolbar .icon-button { width: 32px; height: 32px; padding: 6px; }
.runner-detail-heading { margin-bottom: 14px; }
.runner-detail-heading h1 { font-size: 24px; overflow-wrap: anywhere; }
.runner-detail { display: grid; gap: 12px; }
.runner-detail .content-panel { min-width: 0; padding: 16px 18px; }
.runner-detail h2 { margin: 0 0 12px; font-size: 16px; }
.runner-basic-fields { margin: 0; font-size: 14px; }
.runner-basic-fields > div { display: grid; grid-template-columns: 128px minmax(0, 1fr); gap: 12px; padding: 3px 0; line-height: 1.5; }
.runner-basic-fields dt { color: var(--muted); overflow-wrap: anywhere; }
.runner-basic-fields dd { margin: 0; overflow-wrap: anywhere; }
@media (max-width: 560px) {
  .runner-basic-fields > div { grid-template-columns: 100px minmax(0, 1fr); gap: 12px; }
}
.runner-metadata-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; }
.runner-metadata-grid > * { min-width: 0; }
.taints-table { width: 100%; table-layout: fixed; border-collapse: collapse; text-align: left; }
.taints-table th, .taints-table td { padding: 6px; border-bottom: 1px solid var(--line); vertical-align: top; overflow-wrap: anywhere; font-size: 13px; }
.taints-table th { color: var(--muted); font-size: 13px; font-weight: 500; }
.taints-table tbody tr:last-child td { border-bottom: 0; }
@media (max-width: 1000px) {
  .runner-metadata-grid { grid-template-columns: 1fr; }
}
.resource-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(240px, 100%), 1fr)); gap: 10px; margin-top: 12px; }
.resource-card { min-width: 0; padding: 12px 14px; border: 1px solid var(--line); border-radius: 8px; background: #f8faff; }
.resource-card h3 { margin: 0 0 10px; font-size: 14px; font-weight: 600; overflow-wrap: anywhere; }
.resource-values { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; margin: 0; }
.resource-values dt { margin-bottom: 4px; color: var(--muted); font-size: 12px; }
.resource-values dd { margin: 0; font-size: 18px; font-weight: 600; font-variant-numeric: tabular-nums; line-height: 1.4; overflow-wrap: anywhere; }
.runner-detail-fields { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(280px, 100%), 1fr)); gap: 10px; margin: 0; }
.runner-detail-fields dt { color: var(--muted); font-size: 13px; margin-bottom: 3px; overflow-wrap: anywhere; }
.runner-detail-fields dd { margin: 0; font-size: 14px; overflow-wrap: anywhere; white-space: pre-wrap; }
.metadata-fields { grid-template-columns: 1fr; }
.runner-detail td { overflow-wrap: anywhere; }
.condition-message { min-width: 200px; max-width: 560px; white-space: pre-wrap; }
</style>
