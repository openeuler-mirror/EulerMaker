<template>
  <div class="detail-toolbar">
    <RouterLink class="back-link" to="/projects"><ArrowLeft /> {{ t("project.back") }}</RouterLink>
    <button v-if="project" class="secondary-button" type="button" @click="exportYaml"><Download />{{ t("project.exportYaml") }}</button>
  </div>
  <div v-if="exportErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(exportErrorKey) }}</div>
  <div v-if="copyErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(copyErrorKey) }}</div>

  <div v-if="loadingProject" class="skeleton-list page-skeleton" :aria-label="t('project.loading')"><span v-for="item in 5" :key="item"></span></div>
  <div v-else-if="projectError" class="inline-error page-error">
    <WarningFilled /><span>{{ projectError }}</span><button type="button" @click="loadProject">{{ t("common.reload") }}</button>
  </div>
  <template v-else-if="project">
    <section class="project-hero">
      <div class="project-hero-copy">
        <div class="title-with-status"><h1>{{ project.spec?.displayName || project.metadata?.name }}</h1><span class="project-id"><code>{{ project.metadata?.name }}</code><button type="button" :class="{ copied: copiedId }" :aria-label="copiedId ? t('project.copied') : t('project.copyProjectId')" :title="copiedId ? t('project.copied') : t('project.copyProjectId')" @click="copyProjectId"><Check v-if="copiedId" /><DocumentCopy v-else /></button></span></div>
        <p>{{ project.spec?.description || t("project.noDescription") }}</p>
      </div>
    </section>

    <nav class="project-tabs" role="tablist" :aria-label="t('project.tabsLabel')">
      <button v-for="tab in tabs" :id="`project-tab-${tab.id}`" :key="tab.id" type="button" role="tab" :aria-selected="activeTab === tab.id" :aria-controls="`project-panel-${tab.id}`" :class="{ active: activeTab === tab.id }" @click="selectTab(tab.id)">{{ tab.label }}</button>
    </nav>

    <section v-if="activeTab === 'overview'" id="project-panel-overview" class="project-tab-panel" role="tabpanel" aria-labelledby="project-tab-overview">
      <section class="metrics compact" :aria-label="t('project.overview')">
        <article class="metric-card"><div class="metric-icon blue"><Camera /></div><div><span>{{ t("project.snapshots") }}</span><strong>{{ resourceValue(snapshots) }}</strong><small>{{ t("project.snapshotsHint") }}</small></div></article>
        <article class="metric-card"><div class="metric-icon violet"><Operation /></div><div><span>{{ t("project.builds") }}</span><strong>{{ resourceValue(builds) }}</strong><small>{{ t("project.buildsHint") }}</small></div></article>
        <article class="metric-card"><div class="metric-icon green"><Tickets /></div><div><span>{{ t("project.jobs") }}</span><strong>{{ resourceValue(jobs) }}</strong><small>{{ t("project.jobsHint") }}</small></div></article>
      </section>

      <section class="detail-grid">
        <article class="content-panel">
          <div class="section-heading"><div><span class="eyebrow">{{ t("project.infoEyebrow") }}</span><h2>{{ t("project.info") }}</h2></div></div>
          <dl class="detail-list">
            <div><dt>{{ t("project.projectName") }}</dt><dd>{{ project.metadata?.name }}</dd></div>
            <div><dt>{{ t("project.displayName") }}</dt><dd>{{ project.spec?.displayName || t("common.emptyValue") }}</dd></div>
            <div><dt>{{ t("project.specBranch") }}</dt><dd>{{ project.spec?.specBranch || t("common.emptyValue") }}</dd></div>
            <div><dt>{{ t("project.createdAt") }}</dt><dd>{{ formatDate(project.metadata?.creationTimestamp) }}</dd></div>
          </dl>
        </article>

        <article class="content-panel">
          <div class="section-heading"><div><span class="eyebrow">{{ t("project.activityEyebrow") }}</span><h2>{{ t("project.recentBuilds") }}</h2></div><button v-if="builds.length" class="text-button" type="button" @click="selectTab('builds')">{{ t("project.viewAllBuilds") }}</button></div>
          <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('project.loadingResources')"><span v-for="item in 4" :key="item"></span></div>
          <div v-else-if="resourcesError" class="inline-error compact-error"><WarningFilled /><span>{{ resourcesError }}</span></div>
          <EmptyState v-else-if="!builds.length" :title="t('project.emptyBuilds')" :description="t('project.emptyBuildsHint')" />
          <div v-else class="activity-list">
            <div v-for="build in builds.slice(0, 6)" :key="build.metadata?.name" class="activity-row">
              <div><strong>{{ build.metadata?.name }}</strong><small>{{ build.spec?.buildType || t("project.unspecifiedType") }} · {{ formatDate(build.status?.startTime) }}</small></div>
              <StatusBadge :value="build.status?.phase" />
            </div>
          </div>
        </article>
      </section>
    </section>

    <section v-else-if="activeTab === 'builds'" id="project-panel-builds" class="project-tab-panel" role="tabpanel" aria-labelledby="project-tab-builds">
      <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('project.loadingResources')"><span v-for="item in 6" :key="item"></span></div>
      <div v-else-if="buildLoadFailed" class="inline-error compact-error"><WarningFilled /><span>{{ t("errors.loadBuilds") }}</span></div>
      <EmptyState v-else-if="!builds.length" :title="t('project.emptyBuilds')" :description="t('project.emptyBuildsHint')" />
      <div v-else class="build-history-layout">
        <article class="content-panel build-history-list-panel">
          <div class="section-heading"><div><h2>{{ t("project.buildHistory") }}</h2></div><span>{{ t("common.count", { count: builds.length }) }}</span></div>
          <div class="build-history-list">
            <button v-for="build in builds" :key="build.metadata?.name" type="button" :class="['build-history-item', { active: selectedBuildName === build.metadata?.name }]" :aria-pressed="selectedBuildName === build.metadata?.name" @click="selectedBuildName = build.metadata?.name || ''">
              <span class="build-history-item-heading"><strong>{{ build.metadata?.name }}</strong><StatusBadge :value="build.status?.phase" /></span>
              <small>{{ build.spec?.buildType || t("project.unspecifiedType") }} · {{ buildTargetLabel(build) }}</small>
              <time>{{ formatDate(build.status?.startTime) }}</time>
            </button>
          </div>
        </article>

        <article class="content-panel build-detail-panel">
          <template v-if="selectedBuild">
            <div class="section-heading"><div><h2>{{ selectedBuild.metadata?.name }}</h2><p>{{ t("project.buildDetailHint") }}</p></div><StatusBadge :value="selectedBuild.status?.phase" /></div>
            <dl class="detail-list build-detail-list">
              <div><dt>{{ t("project.buildType") }}</dt><dd>{{ selectedBuild.spec?.buildType || t("project.unspecifiedType") }}</dd></div>
              <div><dt>{{ t("project.buildTarget") }}</dt><dd>{{ buildTargetLabel(selectedBuild) }}</dd></div>
              <div><dt>{{ t("project.stage") }}</dt><dd>{{ selectedBuild.status?.stage || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.startedAt") }}</dt><dd>{{ formatDate(selectedBuild.status?.startTime) }}</dd></div>
              <div><dt>{{ t("project.finishedAt") }}</dt><dd>{{ formatDate(selectedBuild.status?.endTime) }}</dd></div>
              <div><dt>{{ t("project.resultRepository") }}</dt><dd><code>{{ selectedBuild.status?.repo || t("common.emptyValue") }}</code></dd></div>
              <div><dt>{{ t("project.baseBuild") }}</dt><dd>{{ baseBuildLabel(selectedBuild) }}</dd></div>
            </dl>
            <section class="build-detail-section"><h3>{{ t("project.packages") }}</h3><div v-if="selectedBuild.spec?.packages?.length" class="value-chip-list"><code v-for="item in selectedBuild.spec.packages" :key="item">{{ item }}</code></div><p v-else>{{ t("project.noPackages") }}</p></section>
            <section class="build-detail-section"><h3>{{ t("project.bootstrapRepositories") }}</h3><div v-if="selectedBuild.spec?.bootstrapRepo?.length" class="build-bootstrap-list"><div v-for="(repo, index) in selectedBuild.spec.bootstrapRepo" :key="`${repo.name}-${index}`"><strong>{{ repo.name || t("common.emptyValue") }}</strong><code>{{ repo.repo || t("common.emptyValue") }}</code></div></div><p v-else>{{ t("project.noBootstrapRepositories") }}</p></section>
          </template>
          <EmptyState v-else :title="t('project.selectBuild')" :description="t('project.selectBuildHint')" />
        </article>
      </div>
    </section>

    <section v-else id="project-panel-config" class="project-tab-panel config-layout" role="tabpanel" aria-labelledby="project-tab-config">
      <div class="config-split config-wide-panel">
        <div class="config-left-column">
          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.basicConfig") }}</h2></div></div>
            <dl class="detail-list config-detail-list">
              <div><dt>{{ t("project.displayName") }}</dt><dd>{{ project.spec?.displayName || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.descriptionField") }}</dt><dd>{{ project.spec?.description || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.specBranch") }}</dt><dd><code>{{ project.spec?.specBranch || t("common.emptyValue") }}</code></dd></div>
            </dl>
          </article>

          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.buildTargets") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openTargetEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="targetSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.targetsSaved") }}</div>
            <div v-if="project.spec?.buildTargets?.length" class="project-table-wrap">
              <table class="project-table target-table"><thead><tr><th>{{ t("projects.targetOS") }}</th><th>{{ t("projects.targetArch") }}</th><th>{{ t("project.buildEnabled") }}</th><th>{{ t("project.publishEnabled") }}</th></tr></thead><tbody><tr v-for="(target, index) in project.spec.buildTargets" :key="`${target.os}-${target.arch}-${index}`"><td><strong>{{ target.os || t("common.emptyValue") }}</strong></td><td>{{ target.arch || t("common.emptyValue") }}</td><td><span :class="['boolean-status', { enabled: target.buildFlag }]">{{ booleanLabel(target.buildFlag) }}</span></td><td><span :class="['boolean-status', { enabled: target.publishFlag }]">{{ booleanLabel(target.publishFlag) }}</span></td></tr></tbody></table>
            </div>
            <p v-else class="config-empty">{{ t("project.noBuildTargets") }}</p>
          </article>

          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.bootstrapRepositories") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openBootstrapEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="bootstrapSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.bootstrapSaved") }}</div>
            <div v-if="project.spec?.bootstrapRepo?.length" class="project-table-wrap"><table class="project-table config-table"><thead><tr><th>{{ t("project.repositoryName") }}</th><th>{{ t("project.repositoryAddress") }}</th></tr></thead><tbody><tr v-for="(repo, index) in project.spec.bootstrapRepo" :key="`${repo.name}-${index}`"><td><strong>{{ repo.name || t("common.emptyValue") }}</strong></td><td><code>{{ repo.repo || t("common.emptyValue") }}</code></td></tr></tbody></table></div>
            <p v-else class="config-empty">{{ t("project.noBootstrapRepositories") }}</p>
          </article>
        </div>

        <article class="content-panel payload-panel">
          <div class="section-heading"><div><h2>{{ t("project.buildPayload") }}</h2></div><div class="section-actions"><button v-if="canEditProject && !payloadEditing" class="secondary-button compact-button" type="button" @click="startPayloadEditing"><Edit />{{ t("common.edit") }}</button><template v-else-if="payloadEditing"><button class="secondary-button compact-button" type="button" :disabled="savingPayload" @click="cancelPayloadEditing">{{ t("common.cancel") }}</button><button class="primary-button compact-button" type="button" :disabled="savingPayload" @click="savePayload">{{ savingPayload ? t("common.saving") : t("common.save") }}</button></template></div></div>
          <div v-if="payloadSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.payloadSaved") }}</div>
          <template v-if="payloadEditing">
            <textarea v-model="payloadDraft" class="payload-content payload-editor" spellcheck="false" :aria-label="t('project.buildPayload')"></textarea>
            <div v-if="payloadErrorKey" class="form-error payload-error" role="alert"><WarningFilled />{{ t(payloadErrorKey) }}</div>
          </template>
          <template v-else><pre v-if="project.spec?.buildPayload" class="payload-content">{{ project.spec.buildPayload }}</pre><p v-else class="config-empty">{{ t("project.noBuildPayload") }}</p></template>
        </article>
      </div>

      <article class="content-panel config-wide-panel">
        <div class="section-heading"><div><h2>{{ t("project.packageRepositories") }}</h2></div><span>{{ project.spec?.packageRepos?.length || 0 }}</span></div>
        <div v-if="project.spec?.packageRepos?.length" class="project-table-wrap"><table class="project-table config-table"><thead><tr><th>{{ t("project.repositoryName") }}</th><th>URL</th><th>Git ref</th><th>{{ t("project.buildTargets") }}</th></tr></thead><tbody><tr v-for="(repo, index) in project.spec.packageRepos" :key="`${repo.name}-${index}`"><td><strong>{{ repo.name || t("common.emptyValue") }}</strong></td><td><code>{{ repo.url || t("common.emptyValue") }}</code></td><td>{{ gitRefLabel(repo.ref) }}</td><td>{{ targetListLabel(repo.buildTargets) }}</td></tr></tbody></table></div>
        <p v-else class="config-empty">{{ t("project.noPackageRepositories") }}</p>
      </article>
    </section>

    <ModalDialog v-if="targetEditorOpen" title-id="edit-targets-title" :title="t('project.editBuildTargets')" :close-label="t('common.close')" @close="closeTargetEditor">
      <form class="project-form" @submit.prevent="saveTargets">
        <p class="form-hint">{{ t("project.editTargetsHint") }}</p>
        <div class="editable-target-list">
          <fieldset v-for="(target, index) in editingTargets" :key="index" class="target-fieldset editable-target">
            <legend>{{ t("project.targetNumber", { number: index + 1 }) }}</legend>
            <button class="remove-target-button" type="button" :aria-label="t('project.removeTarget', { number: index + 1 })" :disabled="editingTargets.length === 1 || savingTargets" @click="removeTarget(index)"><Delete /></button>
            <div class="form-grid">
              <label class="field required-field"><span>{{ t("projects.targetOS") }}</span><input v-model.trim="target.os" required list="edit-os-options" autocomplete="off" /></label>
              <label class="field required-field"><span>{{ t("projects.targetArch") }}</span><input v-model.trim="target.arch" required list="edit-arch-options" autocomplete="off" /></label>
            </div>
            <div class="checkbox-row"><label><input v-model="target.buildFlag" type="checkbox" />{{ t("projects.buildFlag") }}</label><label><input v-model="target.publishFlag" type="checkbox" />{{ t("projects.publishFlag") }}</label></div>
          </fieldset>
        </div>
        <datalist id="edit-os-options"><option value="openEuler-24.03-LTS"></option><option value="openEuler-22.03-LTS-SP4"></option></datalist>
        <datalist id="edit-arch-options"><option value="x86_64"></option><option value="aarch64"></option><option value="riscv64"></option></datalist>
        <button class="secondary-button add-target-button" type="button" :disabled="savingTargets" @click="addTarget"><Plus />{{ t("project.addTarget") }}</button>
        <div v-if="targetErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(targetErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingTargets" @click="closeTargetEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingTargets">{{ savingTargets ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="bootstrapEditorOpen" title-id="edit-bootstrap-title" :title="t('project.editBootstrapRepositories')" :close-label="t('common.close')" @close="closeBootstrapEditor">
      <form class="project-form" @submit.prevent="saveBootstrapRepositories">
        <p class="form-hint">{{ t("project.editBootstrapHint") }}</p>
        <div v-if="editingBootstrapRepositories.length" class="editable-target-list">
          <fieldset v-for="(repo, index) in editingBootstrapRepositories" :key="index" class="target-fieldset editable-target">
            <legend>{{ t("project.bootstrapNumber", { number: index + 1 }) }}</legend>
            <button class="remove-target-button" type="button" :aria-label="t('project.removeBootstrap', { number: index + 1 })" :disabled="savingBootstrap" @click="removeBootstrapRepository(index)"><Delete /></button>
            <div class="form-grid bootstrap-fields">
              <label class="field required-field repository-name-field"><span>{{ t("project.repositoryName") }}</span><input v-model.trim="repo.name" required autocomplete="off" :placeholder="t('project.repositoryNamePlaceholder')" /></label>
              <label class="field required-field"><span>{{ t("project.repositoryAddress") }}</span><input v-model.trim="repo.repo" required autocomplete="off" :placeholder="t('project.repositoryAddressPlaceholder')" /></label>
            </div>
          </fieldset>
        </div>
        <p v-else class="config-empty editor-empty">{{ t("project.noBootstrapDraft") }}</p>
        <button class="secondary-button add-target-button" type="button" :disabled="savingBootstrap" @click="addBootstrapRepository"><Plus />{{ t("project.addBootstrap") }}</button>
        <div v-if="bootstrapErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(bootstrapErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingBootstrap" @click="closeBootstrapEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingBootstrap">{{ savingBootstrap ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>
  </template>
</template>

<script setup lang="ts">
import { ArrowLeft, Camera, Check, CircleCheckFilled, Delete, DocumentCopy, Download, Edit, Operation, Plus, Tickets, WarningFilled } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import { stringify } from "yaml";

import { errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import StatusBadge from "@/components/StatusBadge.vue";
import { useSessionStore } from "@/stores/session";
import type { BootstrapRepo, Build, BuildTarget, GitRef, Job, Project, Snapshot } from "@/types";

type ProjectTab = "overview" | "builds" | "config";

const route = useRoute();
const router = useRouter();
const session = useSessionStore();
const { t } = useI18n();
const name = computed(() => String(route.params.name || ""));
const project = ref<Project | null>(null);
const snapshots = ref<Snapshot[]>([]);
const builds = ref<Build[]>([]);
const selectedBuildName = ref("");
const jobs = ref<Job[]>([]);
const loadingProject = ref(true);
const resourcesLoading = ref(true);
const projectErrorKey = ref("");
const resourceFailureCount = ref(0);
const buildLoadFailed = ref(false);
const exportErrorKey = ref("");
const copyErrorKey = ref("");
const copiedId = ref(false);
let copyResetTimer: number | undefined;
const targetEditorOpen = ref(false);
const editingTargets = ref<BuildTarget[]>([]);
const savingTargets = ref(false);
const targetErrorKey = ref("");
const targetSaveSuccess = ref(false);
const bootstrapEditorOpen = ref(false);
const editingBootstrapRepositories = ref<BootstrapRepo[]>([]);
const savingBootstrap = ref(false);
const bootstrapErrorKey = ref("");
const bootstrapSaveSuccess = ref(false);
const payloadEditing = ref(false);
const payloadDraft = ref("");
const savingPayload = ref(false);
const payloadErrorKey = ref("");
const payloadSaveSuccess = ref(false);
const activeTab = ref<ProjectTab>(normalizeTab(route.query.tab));
const projectError = computed(() => (projectErrorKey.value ? t(projectErrorKey.value) : ""));
const resourcesError = computed(() =>
  resourceFailureCount.value ? t("project.partialFailure", { count: resourceFailureCount.value }) : "",
);
const tabs = computed<Array<{ id: ProjectTab; label: string }>>(() => [
  { id: "overview", label: t("project.overviewTab") },
  { id: "builds", label: t("project.buildHistoryTab") },
  { id: "config", label: t("project.configTab") },
]);
const canEditProject = computed(() => {
  const identity = session.session?.identity;
  if (!identity || identity.type === "ops") return false;
  if (identity.type === "admin" || identity.scopes.includes("ebs:system")) return true;
  return project.value?.metadata?.labels?.["ebs.io/owner-user"] === identity.name;
});
const selectedBuild = computed(() => builds.value.find((build) => build.metadata?.name === selectedBuildName.value) || null);

onMounted(async () => {
  await loadProject();
  if (project.value) await loadResources();
});

onBeforeUnmount(() => window.clearTimeout(copyResetTimer));

async function loadProject(): Promise<void> {
  loadingProject.value = true;
  projectErrorKey.value = "";
  try {
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`);
  } catch (reason) {
    projectErrorKey.value = errorTranslationKey(reason, "errors.loadProject");
  } finally {
    loadingProject.value = false;
  }
}

async function loadResources(): Promise<void> {
  resourcesLoading.value = true;
  const base = `/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`;
  const results = await Promise.allSettled([
    list<Snapshot>(`${base}/snapshots?limit=20`),
    list<Build>(`${base}/builds?limit=100`),
    list<Job>(`${base}/jobs?limit=20`),
  ]);
  if (results[0].status === "fulfilled") snapshots.value = results[0].value.items;
  if (results[1].status === "fulfilled") {
    builds.value = results[1].value.items;
    if (!builds.value.some((build) => build.metadata?.name === selectedBuildName.value)) {
      selectedBuildName.value = builds.value[0]?.metadata?.name || "";
    }
  }
  if (results[2].status === "fulfilled") jobs.value = results[2].value.items;
  const failures = results.filter((item) => item.status === "rejected");
  buildLoadFailed.value = results[1].status === "rejected";
  resourceFailureCount.value = failures.length;
  resourcesLoading.value = false;
}

function selectTab(tab: ProjectTab): void {
  activeTab.value = tab;
  const query = { ...route.query };
  if (tab === "overview") delete query.tab;
  else query.tab = tab;
  void router.replace({ query });
}

function openTargetEditor(): void {
  if (!project.value || !canEditProject.value) return;
  targetSaveSuccess.value = false;
  targetErrorKey.value = "";
  editingTargets.value = (project.value.spec?.buildTargets || []).map((target) => ({
    os: target.os || "",
    arch: target.arch || "",
    buildFlag: Boolean(target.buildFlag),
    publishFlag: Boolean(target.publishFlag),
  }));
  if (!editingTargets.value.length) addTarget();
  targetEditorOpen.value = true;
}

function closeTargetEditor(): void {
  if (savingTargets.value) return;
  targetEditorOpen.value = false;
  targetErrorKey.value = "";
}

function addTarget(): void {
  editingTargets.value.push({ os: "openEuler-24.03-LTS", arch: "x86_64", buildFlag: true, publishFlag: false });
}

function removeTarget(index: number): void {
  if (editingTargets.value.length > 1) editingTargets.value.splice(index, 1);
}

async function saveTargets(): Promise<void> {
  if (!project.value) return;
  const targets = editingTargets.value.map((target) => ({
    os: target.os?.trim(),
    arch: target.arch?.trim(),
    buildFlag: Boolean(target.buildFlag),
    publishFlag: Boolean(target.publishFlag),
  }));
  if (!targets.length || targets.some((target) => !target.os || !target.arch)) {
    targetErrorKey.value = "projects.targetRequired";
    return;
  }
  savingTargets.value = true;
  targetErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, buildTargets: targets };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    targetEditorOpen.value = false;
    targetSaveSuccess.value = true;
  } catch (reason) {
    targetErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingTargets.value = false;
  }
}

function openBootstrapEditor(): void {
  if (!project.value || !canEditProject.value) return;
  bootstrapSaveSuccess.value = false;
  bootstrapErrorKey.value = "";
  editingBootstrapRepositories.value = (project.value.spec?.bootstrapRepo || []).map((repo) => ({
    name: repo.name || "",
    repo: repo.repo || "",
  }));
  if (!editingBootstrapRepositories.value.length) addBootstrapRepository();
  bootstrapEditorOpen.value = true;
}

function closeBootstrapEditor(): void {
  if (savingBootstrap.value) return;
  bootstrapEditorOpen.value = false;
  bootstrapErrorKey.value = "";
}

function addBootstrapRepository(): void {
  editingBootstrapRepositories.value.push({ name: "", repo: "" });
}

function removeBootstrapRepository(index: number): void {
  editingBootstrapRepositories.value.splice(index, 1);
}

async function saveBootstrapRepositories(): Promise<void> {
  if (!project.value) return;
  const repositories = editingBootstrapRepositories.value.map((repo) => ({ name: repo.name?.trim(), repo: repo.repo?.trim() }));
  if (repositories.some((repo) => !repo.name || !repo.repo)) {
    bootstrapErrorKey.value = "project.bootstrapRequiredFields";
    return;
  }
  savingBootstrap.value = true;
  bootstrapErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, bootstrapRepo: repositories };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    bootstrapEditorOpen.value = false;
    bootstrapSaveSuccess.value = true;
  } catch (reason) {
    bootstrapErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingBootstrap.value = false;
  }
}

function startPayloadEditing(): void {
  if (!project.value || !canEditProject.value) return;
  payloadDraft.value = project.value.spec?.buildPayload || "";
  payloadErrorKey.value = "";
  payloadSaveSuccess.value = false;
  payloadEditing.value = true;
}

function cancelPayloadEditing(): void {
  if (savingPayload.value) return;
  payloadEditing.value = false;
  payloadErrorKey.value = "";
}

async function savePayload(): Promise<void> {
  if (!project.value) return;
  savingPayload.value = true;
  payloadErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, buildPayload: payloadDraft.value };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    payloadEditing.value = false;
    payloadSaveSuccess.value = true;
  } catch (reason) {
    payloadErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingPayload.value = false;
  }
}

function normalizeTab(value: unknown): ProjectTab {
  return value === "builds" || value === "config" ? value : "overview";
}

function exportYaml(): void {
  if (!project.value) return;
  exportErrorKey.value = "";
  try {
    const yaml = stringify(project.value, { indent: 2, lineWidth: 0 });
    const blob = new Blob([yaml], { type: "application/yaml;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `${project.value.metadata?.name || "project"}.yaml`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  } catch {
    exportErrorKey.value = "project.exportFailed";
  }
}

async function copyProjectId(): Promise<void> {
  const value = project.value?.metadata?.name;
  if (!value) return;
  copyErrorKey.value = "";
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(value);
    else fallbackCopy(value);
    copiedId.value = true;
    window.clearTimeout(copyResetTimer);
    copyResetTimer = window.setTimeout(() => {
      copiedId.value = false;
    }, 1800);
  } catch {
    copyErrorKey.value = "project.copyFailed";
  }
}

function fallbackCopy(value: string): void {
  const input = document.createElement("textarea");
  input.value = value;
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.appendChild(input);
  input.select();
  const copied = document.execCommand("copy");
  input.remove();
  if (!copied) throw new Error("copy failed");
}

function resourceValue(items: unknown[]): string | number {
  return resourcesLoading.value ? t("common.emptyValue") : items.length;
}

function buildTargetLabel(build: Build): string {
  const target = build.spec?.buildTarget;
  return targetListLabel(target ? [target] : []);
}

function baseBuildLabel(build: Build): string {
  const base = build.status?.baseBuildRef;
  return [base?.name, base?.repo].filter(Boolean).join(" · ") || t("common.emptyValue");
}

function targetListLabel(targets?: BuildTarget[]): string {
  if (!targets?.length) return t("common.emptyValue");
  return targets.map((target) => [target.os, target.arch].filter(Boolean).join(" / ")).filter(Boolean).join("，") || t("common.emptyValue");
}

function gitRefLabel(ref?: GitRef): string {
  return [ref?.type, ref?.value].filter(Boolean).join(" / ") || t("common.emptyValue");
}

function booleanLabel(value?: boolean): string {
  return value ? t("common.yes") : t("common.no");
}

function formatDate(value?: string): string {
  if (!value) return t("common.emptyValue");
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return t("common.emptyValue");
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
</script>
