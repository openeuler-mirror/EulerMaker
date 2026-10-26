<template>
  <section class="api-docs-page">
    <header class="api-docs-header">
      <div><h1>{{ t("apiDocs.title") }}</h1><p>{{ t("apiDocs.description") }}</p></div>
      <a href="/openapi.json" download="openapi.json">{{ t("apiDocs.source") }}</a>
    </header>
    <iframe :key="locale" :src="`/api-docs-viewer.html?lang=${locale}`" :title="t('apiDocs.title')" />
  </section>
</template>

<script setup lang="ts">
import { useI18n } from "vue-i18n";

const { t, locale } = useI18n();
</script>

<style scoped>
.api-docs-page { display: grid; gap: 20px; }
.api-docs-header { display: flex; align-items: center; justify-content: space-between; gap: 20px; }
h1 { margin: 0 0 10px; font-size: 26px; }
p { margin: 0; color: #667085; line-height: 1.6; }
a { flex-shrink: 0; color: #3868d9; }
iframe { width: 100%; height: max(70vh, 640px); border: 1px solid #e2e7ef; border-radius: 12px; background: #fff; }
@media (max-width: 640px) { .api-docs-header { align-items: flex-start; flex-direction: column; } }
</style>
