<template>
  <section class="api-docs-page">
    <iframe :key="locale" :src="`/api-docs-viewer.html?lang=${locale}`" :title="t('apiDocs.title')" @load="resizeViewer" />
  </section>
</template>

<script setup lang="ts">
import { onBeforeUnmount } from "vue";
import { useI18n } from "vue-i18n";

import { fitApiDocsFrame } from "@/utils/apiDocs";

const { t, locale } = useI18n();
let stopResizing: (() => void) | undefined;

function resizeViewer(event: Event) {
  stopResizing?.();
  stopResizing = fitApiDocsFrame(event.target as HTMLIFrameElement);
}

onBeforeUnmount(() => stopResizing?.());
</script>

<style scoped>
.api-docs-page { text-align: left; }
iframe { display: block; width: 100%; height: 160px; border: 0; }
</style>
