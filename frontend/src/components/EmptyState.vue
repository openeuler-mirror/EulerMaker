<template>
  <div class="empty-state">
    <FolderOpened />
    <strong>{{ title }}</strong>
    <p>{{ description }}</p>
  </div>
</template>

<script setup lang="ts">
import { FolderOpened } from "@element-plus/icons-vue";

defineProps<{ title: string; description: string }>();
</script>
