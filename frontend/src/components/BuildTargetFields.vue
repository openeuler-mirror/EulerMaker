<template>
  <div class="form-grid">
    <label class="field required-field"><span>{{ t('projects.targetOS') }}</span><select :value="os" required :disabled="loading" @change="changeOS"><option value="" disabled>{{ t('buildConf.selectOS') }}</option><option v-if="allowLegacy && os && !operatingSystems.includes(os)" :value="os">{{ os }} — {{ t('buildConf.unsupported') }}</option><option v-for="value in operatingSystems" :key="value" :value="value">{{ value }}</option></select></label>
    <label class="field required-field"><span>{{ t('projects.targetArch') }}</span><select :value="arch" required :disabled="loading" @change="emit('update:arch', ($event.target as HTMLSelectElement).value)"><option value="" disabled>{{ t('buildConf.selectArch') }}</option><option v-if="allowLegacy && arch && !arches(os).includes(arch)" :value="arch">{{ arch }} — {{ t('buildConf.unsupported') }}</option><option v-for="value in arches(os)" :key="value" :value="value">{{ value }}</option></select></label>
  </div>
  <p v-if="error" class="form-error" role="alert">{{ t(error) }} <button type="button" @click="reload">{{ t('common.reload') }}</button></p>
  <p v-else-if="loading" class="form-hint">{{ t('common.loading') }}</p>
  <p v-else-if="!operatingSystems.length" class="form-hint">{{ t('buildConf.empty') }}</p>
</template>
<script setup lang="ts">
import { useI18n } from "vue-i18n";
import { useBuildConf } from "@/composables/useBuildConf";
const props = defineProps<{ os?: string; arch?: string; allowLegacy?: boolean }>();
const emit = defineEmits<{ 'update:os': [string]; 'update:arch': [string] }>();
const { t } = useI18n();
const { loading, error, reload, operatingSystems, arches } = useBuildConf();
function changeOS(event: Event) {
  const os = (event.target as HTMLSelectElement).value;
  emit('update:os', os);
  if (!arches(os).includes(props.arch || '')) emit('update:arch', '');
}
</script>
