<template>
  <section class="page-heading">
    <div><h1>{{ t("projects.title") }}</h1><p>{{ t("projects.description") }}</p></div>
    <div class="page-actions">
      <button class="secondary-button" type="button" :disabled="createForbidden" :title="createForbidden ? t('projects.noPermission') : ''" @click="openDialog('import')"><Upload />{{ t("projects.importYaml") }}</button>
      <button class="primary-button" type="button" :disabled="createForbidden" :title="createForbidden ? t('projects.noPermission') : ''" @click="openDialog('create')"><Plus />{{ t("projects.create") }}</button>
      <button class="icon-button" type="button" :aria-label="t('common.refresh')" :disabled="loading" @click="reload"><Refresh /></button>
    </div>
  </section>

  <div v-if="successKey" class="success-banner" role="status"><CircleCheckFilled />{{ t(successKey, { name: createdName }) }}</div>
  <div v-if="actionErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(actionErrorKey) }}</div>

  <section class="content-panel">
    <div class="list-toolbar">
      <label class="search-box">
        <Search />
        <input v-model.trim="keyword" type="search" :placeholder="t('projects.search')" />
      </label>
    </div>

    <div v-if="loading" class="skeleton-list" :aria-label="t('projects.loading')"><span v-for="item in 6" :key="item"></span></div>
    <div v-else-if="error" class="inline-error">
      <WarningFilled /><span>{{ error }}</span><button type="button" @click="loadPage(currentToken)">{{ t("common.reload") }}</button>
    </div>
    <EmptyState v-else-if="!filtered.length" :title="t('projects.noMatch')" :description="t('projects.noMatchHint')" />
    <div v-else class="project-table-wrap">
      <table class="project-table">
        <thead><tr><th>{{ t("projects.project") }}</th><th>{{ t("projects.descriptionColumn") }}</th><th>{{ t("projects.targets") }}</th><th>{{ t("projects.repositories") }}</th><th>{{ t("projects.createdAt") }}</th></tr></thead>
        <tbody>
          <tr v-for="project in filtered" :key="project.metadata?.name">
            <td><RouterLink class="project-name-link" :to="`/projects/${encodeURIComponent(project.metadata?.name || '')}`"><strong>{{ project.spec?.displayName || project.metadata?.name }}</strong><small>{{ project.metadata?.name }}</small></RouterLink></td>
            <td class="project-description" :title="project.spec?.description || ''">{{ project.spec?.description || t("common.emptyValue") }}</td>
            <td>{{ targetLabel(project) }}</td>
            <td>{{ project.spec?.packageRepos?.length || 0 }}</td>
            <td>{{ formatDate(project.metadata?.creationTimestamp) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="!loading && !error" class="table-footer">
      <div class="page-summary">
        <span>{{ t("common.count", { count: filtered.length }) }}</span>
        <label><select v-model.number="pageSize" :aria-label="t('common.perPage')" @change="changePageSize"><option v-for="size in pageSizes" :key="size" :value="size">{{ t("common.itemsPerPage", { count: size }) }}</option></select></label>
        <nav class="pagination-row" :aria-label="t('common.pagination')">
          <button class="page-button arrow-button" type="button" :aria-label="t('common.previous')" :disabled="currentPage === 1 || loading" @click="goToPage(currentPage - 1)"><ArrowLeft /></button>
          <template v-for="item in paginationItems" :key="item.key">
            <span v-if="item.page === null" class="page-ellipsis">…</span>
            <button v-else class="page-button" :class="{ active: item.page === currentPage }" type="button" :aria-current="item.page === currentPage ? 'page' : undefined" :disabled="loading" @click="goToPage(item.page)">{{ item.page }}</button>
          </template>
          <button class="page-button arrow-button" type="button" :aria-label="t('common.next')" :disabled="!nextToken || loading" @click="goToPage(currentPage + 1)"><ArrowRight /></button>
        </nav>
      </div>
    </div>
  </section>

  <ModalDialog v-if="dialog === 'create'" title-id="create-project-title" :title="t('projects.createTitle')" :eyebrow="t('projects.createEyebrow')" :close-label="t('common.close')" @close="closeDialog">
    <form class="project-form" @submit.prevent="submitForm">
      <div class="form-grid">
        <label class="field required-field"><span>{{ t("projects.name") }}</span><input v-model.trim="form.name" required maxlength="63" autocomplete="off" :placeholder="t('projects.namePlaceholder')" /></label>
        <label class="field"><span>{{ t("projects.displayName") }}</span><input v-model.trim="form.displayName" autocomplete="off" :placeholder="t('projects.displayNamePlaceholder')" /></label>
      </div>
      <label class="field"><span>{{ t("projects.descriptionField") }}</span><textarea v-model.trim="form.description" rows="3" :placeholder="t('projects.descriptionPlaceholder')"></textarea></label>
      <div class="form-grid">
        <label class="field"><span>{{ t("project.refType") }}</span><select v-model="form.defaultRef.type"><option value="Branch">{{ t("project.refBranch") }}</option><option value="Tag">{{ t("project.refTag") }}</option></select></label>
        <label class="field"><span>{{ t("projects.defaultRef") }}</span><input v-model.trim="form.defaultRef.value" required autocomplete="off" /></label>
      </div>
      <label v-if="requiresOwner" class="field required-field"><span>{{ t("projects.ownerUser") }}</span><input v-model.trim="form.ownerUser" required autocomplete="off" :placeholder="t('projects.ownerUserPlaceholder')" /></label>
      <fieldset class="target-fieldset">
        <legend>{{ t("projects.firstTarget") }}</legend>
        <div class="form-grid">
          <label class="field required-field"><span>{{ t("projects.targetOS") }}</span><input v-model.trim="form.os" required list="os-options" autocomplete="off" /></label>
          <label class="field required-field"><span>{{ t("projects.targetArch") }}</span><input v-model.trim="form.arch" required list="arch-options" autocomplete="off" /></label>
        </div>
        <div class="checkbox-row">
          <label><input v-model="form.buildFlag" type="checkbox" />{{ t("projects.buildFlag") }}</label>
          <label><input v-model="form.publishFlag" type="checkbox" />{{ t("projects.publishFlag") }}</label>
        </div>
      </fieldset>
      <datalist id="os-options"><option value="openEuler-24.03-LTS"></option><option value="openEuler-22.03-LTS-SP4"></option></datalist>
      <datalist id="arch-options"><option value="x86_64"></option><option value="aarch64"></option><option value="riscv64"></option></datalist>
      <div v-if="submitErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(submitErrorKey) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" :disabled="submitting" @click="closeDialog">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="submitting">{{ submitting ? t("projects.submitting") : t("projects.submitCreate") }}</button></div>
    </form>
  </ModalDialog>

  <ModalDialog v-if="dialog === 'import'" title-id="import-project-title" :title="t('projects.importTitle')" :eyebrow="t('projects.importEyebrow')" :close-label="t('common.close')" @close="closeDialog">
    <form class="project-form" @submit.prevent="submitYaml">
      <p class="form-hint">{{ t("projects.importHint") }}</p>
      <label class="file-button secondary-button"><Upload />{{ t("projects.chooseFile") }}<input type="file" accept=".yaml,.yml,text/yaml,application/yaml" @change="readYamlFile" /></label>
      <span v-if="selectedFileName" class="selected-file">{{ selectedFileName }}</span>
      <label class="field required-field"><span>{{ t("projects.yamlContent") }}</span><textarea v-model="yamlSource" class="yaml-editor" required spellcheck="false" :placeholder="t('projects.yamlPlaceholder')"></textarea></label>
      <div v-if="submitErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(submitErrorKey) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" :disabled="submitting" @click="closeDialog">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="submitting">{{ submitting ? t("projects.submitting") : t("projects.submitImport") }}</button></div>
    </form>
  </ModalDialog>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, CircleCheckFilled, Plus, Refresh, Search, Upload, WarningFilled } from "@element-plus/icons-vue";
import { computed, onMounted, reactive, ref } from "vue";
import { RouterLink, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";

import { createProject, errorTranslationKey, list } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import { useSessionStore } from "@/stores/session";
import type { Project } from "@/types";
import { ProjectManifestError, projectFromForm, projectFromYaml } from "@/utils/projectManifest";

const projects = ref<Project[]>([]);
const { t } = useI18n();
const router = useRouter();
const session = useSessionStore();
const loading = ref(true);
const errorKey = ref("");
const error = computed(() => (errorKey.value ? t(errorKey.value) : ""));
const keyword = ref("");
const currentToken = ref("");
const nextToken = ref("");
const pageTokens = ref<string[]>([""]);
const currentPage = ref(1);
const remainingCount = ref<number>();
const pageSizes = [10, 20, 50, 100] as const;
const pageSize = ref<number>(20);
const dialog = ref<"create" | "import" | "">("");
const submitting = ref(false);
const submitErrorKey = ref("");
const actionErrorKey = ref("");
const successKey = ref("");
const createdName = ref("");
const yamlSource = ref("");
const selectedFileName = ref("");
const form = reactive({
  name: "",
  displayName: "",
  description: "",
  defaultRef: { type: "Branch" as "Branch" | "Tag", value: "master" },
  os: "openEuler-24.03-LTS",
  arch: "x86_64",
  buildFlag: true,
  publishFlag: false,
  ownerUser: "",
});
const canCreate = computed(() => {
  const identity = session.session?.identity;
  return identity?.type === "user" || identity?.type === "admin" || Boolean(identity?.scopes.includes("ebs:system"));
});
const createForbidden = computed(() => session.authenticated && !canCreate.value);
const requiresOwner = computed(() => session.role === "admin" || Boolean(session.session?.identity.scopes.includes("ebs:system")));
const totalPages = computed(() => {
  const estimated = remainingCount.value === undefined ? 0 : currentPage.value + Math.ceil(remainingCount.value / pageSize.value);
  return Math.max(1, estimated, pageTokens.value.length, currentPage.value + (nextToken.value ? 1 : 0));
});
const paginationItems = computed(() => buildPaginationItems(totalPages.value, currentPage.value));
const filtered = computed(() => {
  const value = keyword.value.toLowerCase();
  if (!value) return projects.value;
  return projects.value.filter((item) =>
    [item.metadata?.name, item.spec?.displayName, item.spec?.description].some((text) => text?.toLowerCase().includes(value)),
  );
});

onMounted(() => loadPage("", 1));

async function loadPage(token: string, page = currentPage.value): Promise<boolean> {
  loading.value = true;
  errorKey.value = "";
  const query = new URLSearchParams({ limit: String(pageSize.value) });
  if (token) query.set("continue", token);
  try {
    const result = await list<Project>(`/apis/ebs/v1/projects?${query}`);
    projects.value = result.items;
    nextToken.value = result.next;
    remainingCount.value = result.remaining;
    currentToken.value = token;
    currentPage.value = page;
    if (result.next) pageTokens.value[page] = result.next;
    else pageTokens.value = pageTokens.value.slice(0, page);
    return true;
  } catch (reason) {
    errorKey.value = errorTranslationKey(reason, "errors.loadProjects");
    return false;
  } finally {
    loading.value = false;
  }
}

function reload(): void {
  resetPagination();
  void loadPage("", 1);
}

function changePageSize(): void {
  resetPagination();
  void loadPage("", 1);
}

function openDialog(kind: "create" | "import"): void {
  actionErrorKey.value = "";
  successKey.value = "";
  if (!session.authenticated) {
    void router.push({ name: "login", query: { redirect: "/projects" } });
    return;
  }
  if (!canCreate.value) {
    actionErrorKey.value = "projects.noPermission";
    return;
  }
  submitErrorKey.value = "";
  dialog.value = kind;
}

function closeDialog(): void {
  if (submitting.value) return;
  dialog.value = "";
  submitErrorKey.value = "";
}

async function submitForm(): Promise<void> {
  try {
    await submitProject(projectFromForm(form), "projects.createSuccess");
  } catch (reason) {
    setSubmitError(reason);
  }
}

async function submitYaml(): Promise<void> {
  try {
    await submitProject(projectFromYaml(yamlSource.value), "projects.importSuccess");
  } catch (reason) {
    setSubmitError(reason);
  }
}

async function submitProject(project: Project, messageKey: string): Promise<void> {
  submitting.value = true;
  submitErrorKey.value = "";
  try {
    const created = await createProject(project);
    const name = created.metadata?.name || project.metadata?.name || "";
    dialog.value = "";
    createdName.value = name;
    successKey.value = messageKey;
    resetPagination();
    await loadPage("", 1);
    if (name) await router.push({ name: "project", params: { name } });
  } finally {
    submitting.value = false;
  }
}

function setSubmitError(reason: unknown): void {
  submitErrorKey.value = reason instanceof ProjectManifestError ? reason.translationKey : errorTranslationKey(reason, "errors.requestFailed");
}

async function readYamlFile(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  if (!file) return;
  submitErrorKey.value = "";
  selectedFileName.value = file.name;
  try {
    yamlSource.value = await file.text();
  } catch {
    submitErrorKey.value = "projects.fileReadFailed";
  }
}

async function goToPage(page: number): Promise<void> {
  if (page < 1 || page > totalPages.value || page === currentPage.value || loading.value) return;
  const knownToken = pageTokens.value[page - 1];
  if (knownToken !== undefined) {
    await loadPage(knownToken, page);
    return;
  }
  while (currentPage.value < page && nextToken.value) {
    const nextPage = currentPage.value + 1;
    if (!(await loadPage(nextToken.value, nextPage))) return;
  }
}

function resetPagination(): void {
  pageTokens.value = [""];
  currentPage.value = 1;
  currentToken.value = "";
  nextToken.value = "";
  remainingCount.value = undefined;
}

function buildPaginationItems(total: number, current: number): Array<{ key: string; page: number | null }> {
  const pages = new Set<number>();
  if (total <= 7) {
    for (let page = 1; page <= total; page += 1) pages.add(page);
  } else if (current <= 4) {
    for (let page = 1; page <= 5; page += 1) pages.add(page);
    pages.add(total);
  } else if (current >= total - 3) {
    pages.add(1);
    for (let page = total - 4; page <= total; page += 1) pages.add(page);
  } else {
    pages.add(1);
    pages.add(current - 1);
    pages.add(current);
    pages.add(current + 1);
    pages.add(total);
  }
  const sorted = [...pages].sort((left, right) => left - right);
  const result: Array<{ key: string; page: number | null }> = [];
  sorted.forEach((page, index) => {
    if (index > 0 && page - sorted[index - 1] > 1) result.push({ key: `ellipsis-${page}`, page: null });
    result.push({ key: `page-${page}`, page });
  });
  return result;
}

function targetLabel(project: Project): string {
  const targets = project.spec?.buildTargets || [];
  if (!targets.length) return t("common.emptyValue");
  return targets.slice(0, 2).map((item) => [item.os, item.arch].filter(Boolean).join(" / ")).join(" · ");
}

function formatDate(value?: string): string {
  if (!value) return t("common.emptyValue");
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return t("common.emptyValue");
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
</script>
