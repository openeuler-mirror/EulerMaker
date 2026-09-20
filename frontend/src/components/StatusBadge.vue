<template>
  <span :class="['status-badge', tone]">
    <span class="status-dot" aria-hidden="true"></span>
    {{ label }}
  </span>
</template>

<script setup lang="ts">
import { computed } from "vue";
import { useI18n } from "vue-i18n";

const props = defineProps<{ value?: string }>();
const { t, te } = useI18n();
const label = computed(() => {
  if (!props.value) return t("common.unknown");
  const key = `status.${props.value.toLowerCase()}`;
  return te(key) ? t(key) : props.value;
});

const tone = computed(() => {
  const value = (props.value || "").toLowerCase();
  if (["active", "success", "succeeded", "completed", "online", "ready"].includes(value)) return "success";
  if (["failed", "aborted", "offline"].includes(value)) return "danger";
  if (["processing", "running", "prepared"].includes(value)) return "primary";
  return "muted";
});
</script>
