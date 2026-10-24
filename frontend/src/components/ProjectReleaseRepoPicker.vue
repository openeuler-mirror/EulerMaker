<template>
  <section class="release-repo-picker" :aria-label="t('project.addProjectRelease')">
    <p class="form-hint">{{ t('project.projectReleaseHint') }}</p>
    <p v-if="loading" role="status">{{ t('common.loading') }}</p>
    <div v-else-if="error" class="form-error" role="alert">
      {{ t(error) }}
      <button class="text-button" type="button" :disabled="disabled" @click="loadProjects">{{ t('common.refresh') }}</button>
    </div>
    <template v-else>
      <div class="release-repo-fields">
        <div class="field"><span>{{ t('project.releaseProject') }}</span><AppSelect v-model="selectedName" :options="projectOptions" :label="t('project.releaseProject')" :placeholder="t('project.selectReleaseProject')" searchable :search-placeholder="t('project.releaseProjectSearch')" :empty-text="t('project.noReleaseProjects')" :disabled="disabled || !projectOptions.length" /></div>
        <div class="field"><span>{{ t('project.releaseOS') }}</span><AppSelect v-model="selectedOS" :options="selectedOSOptions" :label="t('project.releaseOS')" :placeholder="t('project.selectReleaseOS')" :disabled="disabled || !selectedProject" /></div>
      </div>
      <p v-if="!projectOptions.length" class="form-hint">{{ t('project.noReleaseProjects') }}</p>
      <label v-if="url" class="field"><span>{{ t('project.sourceAddress') }}</span><input :value="url" readonly /></label>
      <button class="secondary-button" type="button" :disabled="disabled || !url || duplicate" @click="addRepository">{{ t(duplicate ? 'project.releaseAlreadyAdded' : 'project.addBootstrap') }}</button>
    </template>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { errorTranslationKey, list } from '@/api';
import type { BootstrapRepo, Project } from '@/types';
import AppSelect from './AppSelect.vue';
import { filterReleaseProjects, projectReleaseRepoUrl, releaseOperatingSystems } from '@/utils/projectReleaseRepo';

const props = defineProps<{ currentProject: string; existingUrls: string[]; disabled?: boolean }>();
const emit = defineEmits<{ add: [repo: BootstrapRepo] }>();
const { t } = useI18n();
const projects = ref<Project[]>([]);
const loading = ref(false);
const error = ref('');
const selectedName = ref('');
const selectedOS = ref('');
let generation = 0;
const candidates = computed(() => filterReleaseProjects(projects.value, props.currentProject, '', ''));
const projectOptions = computed(() => candidates.value.map(project => ({ value: project.metadata!.name!, label: project.metadata!.name! })));
const selectedProject = computed(() => candidates.value.find(project => project.metadata?.name === selectedName.value));
const selectedOSOptions = computed(() => (selectedProject.value ? releaseOperatingSystems(selectedProject.value) : []).map(value => ({ value, label: value })));
watch(candidates, () => { if (!selectedProject.value) selectedName.value = ''; });
watch(selectedOSOptions, options => { if (!options.some(option => option.value === selectedOS.value)) selectedOS.value = options.length === 1 ? options[0]!.value : ''; });
const url = computed(() => selectedProject.value ? projectReleaseRepoUrl(selectedProject.value, selectedOS.value) : '');
const duplicate = computed(() => props.existingUrls.some(value => value.trim().replace(/\/+$/, '') === url.value));
function addRepository(): void {
  if (props.disabled || !url.value || duplicate.value) return;
  emit('add', { name: `${selectedName.value}-${selectedOS.value}`, url: url.value });
}
async function loadProjects(): Promise<void> {
  const current = ++generation;
  loading.value = true;
  error.value = '';
  const items: Project[] = [];
  let next = '';
  try {
    do {
      const params = new URLSearchParams({ limit: '100', includeFields: 'metadata,spec.displayName,spec.buildTargets' });
      if (next) params.set('continue', next);
      const result = await list<Project>(`/apis/ebs/v1/projects?${params}`);
      if (current !== generation) return;
      items.push(...result.items);
      next = result.next;
    } while (next);
    projects.value = items;
  } catch (reason) {
    if (current === generation) error.value = errorTranslationKey(reason, 'errors.loadProjects');
  } finally {
    if (current === generation) loading.value = false;
  }
}
onMounted(loadProjects);
onBeforeUnmount(() => { generation++; });
</script>
<style scoped>
.release-repo-picker { display: grid; gap: 12px; padding: 14px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); }
.release-repo-fields { display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; }
.release-repo-picker > .form-hint { margin: 0; }
.release-repo-picker > button { justify-self: start; }
</style>
