<template>
  <div class="form-grid">
    <div class="field required-field"><span>{{ t('projects.targetOS') }}</span><AppSelect :model-value="os || ''" :options="[...(allowLegacy && os && !operatingSystems.includes(os) ? [{ value: os, label: `${os} — ${t('buildConf.unsupported')}` }] : []), ...operatingSystems.map(value => ({ value, label: value }))]" :label="t('projects.targetOS')" :placeholder="t('buildConf.selectOS')" :disabled="loading" required @update:model-value="changeOS" /></div>
    <div class="field required-field"><span>{{ t('projects.targetArch') }}</span><AppSelect :model-value="arch || ''" :options="[...(allowLegacy && arch && !arches(os).includes(arch) ? [{ value: arch, label: `${arch} — ${t('buildConf.unsupported')}` }] : []), ...arches(os).map(value => ({ value, label: value }))]" :label="t('projects.targetArch')" :placeholder="t('buildConf.selectArch')" :disabled="loading" required @update:model-value="emit('update:arch', $event)" /></div>
  </div>
  <p v-if="error" class="form-error" role="alert">{{ t(error) }} <button type="button" @click="reload">{{ t('common.reload') }}</button></p>
  <p v-else-if="loading" class="form-hint">{{ t('common.loading') }}</p>
  <p v-else-if="!operatingSystems.length" class="form-hint">{{ t('buildConf.empty') }}</p>
</template>
<script setup lang="ts">
import { useI18n } from "vue-i18n";
import { useBuildConf } from "@/composables/useBuildConf";
import AppSelect from "@/components/AppSelect.vue";
const props = defineProps<{ os?: string; arch?: string; allowLegacy?: boolean }>();
const emit = defineEmits<{ 'update:os': [string]; 'update:arch': [string] }>();
const { t } = useI18n();
const { loading, error, reload, operatingSystems, arches } = useBuildConf();
function changeOS(os: string) {
  emit('update:os', os);
  if (!arches(os).includes(props.arch || '')) emit('update:arch', '');
}
</script>
