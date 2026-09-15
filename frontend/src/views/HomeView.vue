<template>
  <section class="welcome-panel">
    <div>
      <span class="eyebrow">{{ t("home.eyebrow") }}</span>
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

  <section class="metrics" :aria-label="t('home.overview')">
    <article class="metric-card">
      <div class="metric-icon blue"><Folder /></div>
      <div><span>{{ t("home.projectCount") }}</span><strong>{{ loading ? t("common.emptyValue") : projects.length }}</strong><small>{{ t("home.loaded") }}</small></div>
    </article>
    <article class="metric-card">
      <div class="metric-icon green"><CircleCheck /></div>
      <div><span>{{ t("home.activeProjects") }}</span><strong>{{ loading ? t("common.emptyValue") : activeCount }}</strong><small>{{ t("home.activeHint") }}</small></div>
    </article>
    <article class="metric-card">
      <div class="metric-icon violet"><Cpu /></div>
      <div><span>{{ t("home.buildTargets") }}</span><strong>{{ loading ? t("common.emptyValue") : targetCount }}</strong><small>{{ t("home.targetHint") }}</small></div>
    </article>
  </section>

  <section class="content-panel">
    <div class="section-heading">
      <div><span class="eyebrow">{{ t("home.recentEyebrow") }}</span><h2>{{ t("home.recentProjects") }}</h2></div>
      <RouterLink to="/projects">{{ t("home.allProjects") }} <ArrowRight /></RouterLink>
    </div>

    <div v-if="loading" class="skeleton-list" :aria-label="t('home.loadProjects')"><span v-for="item in 3" :key="item"></span></div>
    <div v-else-if="error" class="inline-error">
      <WarningFilled /><span>{{ error }}</span><button type="button" @click="load">{{ t("common.reload") }}</button>
    </div>
    <EmptyState v-else-if="!projects.length" :title="t('home.emptyTitle')" :description="t('home.emptyDescription')" />
    <div v-else class="project-grid">
      <RouterLink
        v-for="project in projects.slice(0, 6)"
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
import { ArrowRight, CircleCheck, Cpu, Folder, WarningFilled } from "@element-plus/icons-vue";
import { computed, onMounted, ref } from "vue";
import { RouterLink } from "vue-router";
import { useI18n } from "vue-i18n";

import { errorTranslationKey, list } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import StatusBadge from "@/components/StatusBadge.vue";
import type { Project } from "@/types";

const projects = ref<Project[]>([]);
const { t } = useI18n();
const loading = ref(true);
const errorKey = ref("");
const error = computed(() => (errorKey.value ? t(errorKey.value) : ""));
const activeCount = computed(() => projects.value.filter((item) => item.status?.phase === "Active").length);
const targetCount = computed(() => projects.value.reduce((total, item) => total + (item.spec?.buildTargets?.length || 0), 0));

onMounted(load);

async function load(): Promise<void> {
  loading.value = true;
  errorKey.value = "";
  try {
    projects.value = (await list<Project>("/apis/ebs/v1/projects?limit=24")).items;
  } catch (reason) {
    errorKey.value = errorTranslationKey(reason, "errors.loadProjects");
  } finally {
    loading.value = false;
  }
}

function initial(project: Project): string {
  return (project.spec?.displayName || project.metadata?.name || "E").slice(0, 1).toUpperCase();
}
</script>
