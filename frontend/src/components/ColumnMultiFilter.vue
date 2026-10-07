<template>
  <button ref="trigger" class="spec-status-filter-trigger" type="button" :aria-label="label" :aria-expanded="open" :aria-controls="open ? menuId : undefined" @click="toggleMenu">
    {{ label }}<Filter aria-hidden="true" /><span v-if="modelValue.length" class="spec-status-filter-count">{{ modelValue.length }}</span>
  </button>
  <Teleport to="body">
    <div v-if="open" :id="menuId" ref="menu" class="spec-status-filter-menu" :style="menuStyle" role="group" :aria-label="label">
      <label v-if="searchable" class="column-filter-search"><Search aria-hidden="true" /><input v-model="query" type="search" :placeholder="searchPlaceholder" :aria-label="searchPlaceholder" /></label>
      <label class="spec-status-filter-option all" :class="{ selected: !modelValue.length }"><input type="checkbox" :checked="!modelValue.length" @change="emit('update:modelValue', [])" /><span class="spec-status-filter-check" aria-hidden="true"><Check /></span><span>{{ allLabel }}</span></label>
      <label v-for="option in filteredOptions" :key="option.value" class="spec-status-filter-option" :class="{ selected: modelValue.includes(option.value) }" :title="option.label"><input type="checkbox" :checked="modelValue.includes(option.value)" @change="toggleOption(option.value, $event)" /><span class="spec-status-filter-check" aria-hidden="true"><Check /></span><span class="column-filter-option-label">{{ option.label }}</span></label>
      <p v-if="query && !filteredOptions.length" class="column-filter-empty">{{ noResultsLabel }}</p>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { Check, Filter, Search } from '@element-plus/icons-vue';
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, type CSSProperties } from 'vue';

const props = withDefaults(defineProps<{
  label: string;
  allLabel: string;
  options: Array<{ value: string; label: string }>;
  modelValue: string[];
  width?: number;
  searchable?: boolean;
  searchPlaceholder?: string;
  noResultsLabel?: string;
}>(), { width: 210, searchable: false, searchPlaceholder: 'Search', noResultsLabel: 'No matching options' });
const emit = defineEmits<{ 'update:modelValue': [value: string[]] }>();
const trigger = ref<HTMLElement | null>(null);
const menu = ref<HTMLElement | null>(null);
const menuStyle = ref<CSSProperties>({});
const menuId = useId();
const open = ref(false);
const query = ref('');
const filteredOptions = computed(() => {
  const normalized = query.value.trim().toLowerCase();
  return normalized ? props.options.filter((option) => option.label.toLowerCase().includes(normalized)) : props.options;
});

function toggleMenu(): void {
  open.value = !open.value;
  if (!open.value) query.value = '';
  if (open.value) void nextTick(positionMenu);
}

function positionMenu(): void {
  if (!open.value || !trigger.value) return;
  const rect = trigger.value.getBoundingClientRect();
  const height = Math.min(menu.value?.offsetHeight || 240, 240);
  const top = window.innerHeight - rect.bottom < height + 8 && rect.top > height
    ? rect.top - height - 4 : rect.bottom + 4;
  menuStyle.value = {
    top: `${top}px`,
    left: `${Math.max(8, Math.min(rect.right - props.width, window.innerWidth - props.width - 8))}px`,
    width: `${props.width}px`,
  };
}

function toggleOption(value: string, event: Event): void {
  const checked = (event.target as HTMLInputElement).checked;
  emit('update:modelValue', checked
    ? [...props.modelValue, value]
    : props.modelValue.filter((item) => item !== value));
}

function closeOnOutsideClick(event: PointerEvent): void {
  const target = event.target as Node;
  if (!trigger.value?.contains(target) && !menu.value?.contains(target)) {
    open.value = false;
    query.value = '';
  }
}

function closeOnEscape(event: KeyboardEvent): void {
  if (event.key !== 'Escape' || !open.value) return;
  open.value = false;
  query.value = '';
  trigger.value?.focus();
}

onMounted(() => {
  window.addEventListener('pointerdown', closeOnOutsideClick);
  window.addEventListener('keydown', closeOnEscape);
  window.addEventListener('resize', positionMenu);
  window.addEventListener('scroll', positionMenu, true);
});
onBeforeUnmount(() => {
  window.removeEventListener('pointerdown', closeOnOutsideClick);
  window.removeEventListener('keydown', closeOnEscape);
  window.removeEventListener('resize', positionMenu);
  window.removeEventListener('scroll', positionMenu, true);
});
</script>
