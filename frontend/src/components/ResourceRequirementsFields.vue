<template>
  <div class="resource-quantity-grid">
    <template v-for="group in groups" :key="group">
      <label v-for="resource in resources" :key="resource" class="field">
        <span>{{ t('operations.' + group) }} · {{ resource === 'cpu' ? 'CPU' : t('operations.memory') }}</span>
        <input :value="quantity(group, resource)" :placeholder="resource === 'cpu' ? '4 / 500m' : '8Gi'"
          @input="update(group, resource, ($event.target as HTMLInputElement).value)" />
      </label>
    </template>
  </div>
</template>
<script setup lang="ts">
import { useI18n } from "vue-i18n";
import type { JSONMap } from "./buildResourceContentDraft";
const props = defineProps<{ modelValue: JSONMap }>();
const emit = defineEmits<{ 'update:modelValue': [value: JSONMap] }>();
const { t } = useI18n();
const groups = ["requests", "limits"] as const;
const resources = ["cpu", "memory"] as const;
function quantity(group: string, resource: string): string {
  return String((props.modelValue[group] as JSONMap | undefined)?.[resource] ?? "");
}
function update(group: string, resource: string, value: string): void {
  const next = { ...props.modelValue };
  const values = { ...(next[group] as JSONMap | undefined) };
  if (value.trim()) values[resource] = value.trim();
  else delete values[resource];
  if (Object.keys(values).length) next[group] = values;
  else delete next[group];
  emit("update:modelValue", next);
}
</script>
