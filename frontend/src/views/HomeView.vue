<template>
  <section class="welcome-panel">
    <div>
      <h1>{{ t("home.title") }}</h1>
      <p>{{ t("home.description") }}</p>
      <RouterLink class="primary-action" to="/projects">{{ t("home.viewProjects") }} <ArrowRight /></RouterLink>
    </div>
    <div class="build-mark" aria-hidden="true">
      <span class="build-mark-ring"></span>
      <img src="@/assets/eulermaker-logo.svg" alt="" />
      <small>{{ t("home.build") }}</small>
    </div>
  </section>

  <section class="metrics home-metrics" :aria-label="t('home.overview')">
    <article class="metric-card">
      <div class="metric-icon blue"><Folder /></div>
      <div><span>{{ t("home.projectCount") }}</span><strong>{{ loading ? t("common.emptyValue") : projects.length }}</strong><small>{{ t("home.loaded") }}</small></div>
    </article>
  </section>

  <section class="content-panel">
    <div class="section-heading">
      <div><h2>{{ t("home.recentProjects") }}</h2></div>
      <RouterLink to="/projects">{{ t("home.allProjects") }} <ArrowRight /></RouterLink>
    </div>

    <div v-if="loading" class="skeleton-list" :aria-label="t('home.loadProjects')"><span v-for="item in 3" :key="item"></span></div>
    <div v-else-if="error" class="inline-error">
      <WarningFilled /><span>{{ error }}</span><button type="button" @click="load">{{ t("common.reload") }}</button>
    </div>
    <EmptyState v-else-if="!session.authenticated" :title="t('home.signInTitle')" :description="t('home.signInDescription')" />
    <EmptyState v-else-if="!recentProjects.length" :title="t('home.emptyTitle')" :description="t('home.emptyDescription')" />
    <div v-else class="project-grid">
      <RouterLink
        v-for="project in recentProjects"
        :key="project.metadata?.name"
        class="project-card"
        :to="`/projects/${encodeURIComponent(project.metadata?.name || '')}`"
      >
        <div class="project-card-top">
          <span class="project-monogram">{{ initial(project) }}</span>
          <StatusBadge :value="project.status?.phase" />
        </div>
        <h3>{{ project.spec?.displayName || project.metadata?.name }}</h3>
        <p>{{ project.spec?.description || t("home.noDescription") }}</p>
        <div class="project-meta">
          <span>{{ t("home.repositories", { count: project.spec?.packageRepos?.length || 0 }) }}</span>
          <span>{{ t("home.targets", { count: project.spec?.buildTargets?.length || 0 }) }}</span>
        </div>
      </RouterLink>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ArrowRight, Folder, WarningFilled } from "@element-plus/icons-vue";
import { computed, ref, watch } from "vue";
import { RouterLink } from "vue-router";
import { useI18n } from "vue-i18n";

import { errorTranslationKey, list } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import StatusBadge from "@/components/StatusBadge.vue";
import { useSessionStore } from "@/stores/session";
import type { Project } from "@/types";

const projects = ref<Project[]>([]);
const recentProjects = ref<Project[]>([]);
const session = useSessionStore();
const { t } = useI18n();
const loading = ref(true);
const errorKey = ref("");
const error = computed(() => (errorKey.value ? t(errorKey.value) : ""));

let loadVersion = 0;
watch(() => session.username, load, { immediate: true });

async function load(): Promise<void> {
  const version = ++loadVersion;
  const username = session.username;
  loading.value = true;
  errorKey.value = "";
  recentProjects.value = [];
  try {
    const selectors = username ? [
      `ebs.io/owner-user=${username}`,
      `ebs.io/member-user.${username}=true`,
    ] : [];
    const [overview, ...related] = await Promise.all([
      list<Project>("/apis/ebs/v1/projects?limit=24"),
      ...selectors.map(labelSelector => list<Project>(`/apis/ebs/v1/projects?${new URLSearchParams({ limit: "6", labelSelector })}`)),
    ]);
    if (version !== loadVersion) return;
    projects.value = overview.items;
    const unique = new Map<string, Project>();
    for (const project of related.flatMap(page => page.items)) {
      const labels = project.metadata?.labels || {};
      if (project.metadata?.name && (labels["ebs.io/owner-user"] === username || labels[`ebs.io/member-user.${username}`] === "true")) {
        unique.set(project.metadata.name, project);
      }
    }
    const createdAt = (project: Project): number => Date.parse(project.metadata?.creationTimestamp || "") || 0;
    recentProjects.value = [...unique.values()].sort((a, b) =>
      createdAt(b) - createdAt(a) || (a.metadata?.name || "").localeCompare(b.metadata?.name || ""),
    ).slice(0, 6);
  } catch (reason) {
    if (version !== loadVersion) return;
    errorKey.value = errorTranslationKey(reason, "errors.loadProjects");
  } finally {
    if (version === loadVersion) loading.value = false;
  }
}

function initial(project: Project): string {
  return (project.spec?.displayName || project.metadata?.name || "E").slice(0, 1).toUpperCase();
}
</script>
