<template>
  <div ref="root" class="app-select" :class="{ compact }">
    <button ref="trigger" class="app-select-trigger" type="button" role="combobox" aria-haspopup="listbox"
      :aria-label="label" :aria-required="required || undefined" :aria-expanded="open"
      :aria-controls="open ? listId : undefined" :aria-activedescendant="open && activeIndex >= 0 ? `${listId}-option-${activeIndex}` : undefined"
      :disabled="disabled" @click="toggle" @keydown="onKeydown" @blur="onBlur">
      <span :class="{ placeholder: !selectedOption }">{{ selectedOption?.label || (modelValue ? String(modelValue) : placeholder) }}</span>
      <ArrowDown class="app-select-chevron" />
    </button>
    <Teleport to="body">
      <div v-if="open && options.length" ref="menu" class="app-select-menu" :style="menuStyle">
        <div v-if="searchable" class="app-select-search"><input ref="searchInput" v-model="filter" type="search" :placeholder="searchPlaceholder" :aria-label="searchPlaceholder || label" role="combobox" :aria-expanded="open" :aria-controls="listId" :aria-activedescendant="activeIndex >= 0 ? `${listId}-option-${activeIndex}` : undefined" @keydown="onSearchKeydown" @blur="onBlur" /></div>
        <div :id="listId" role="listbox" :aria-label="label">
        <button v-for="(option, index) in visibleOptions" :id="`${listId}-option-${index}`" :key="`${option.value}-${index}`"
          class="app-select-option" :class="{ active: activeIndex === index, selected: modelValue === option.value }"
          type="button" role="option" tabindex="-1" :aria-selected="modelValue === option.value" :disabled="option.disabled"
          @mousedown.prevent @mouseenter="activeIndex = index" @click="choose(index)">
          <span>{{ option.label }}</span><Check v-if="modelValue === option.value" />
        </button>
        </div>
        <p v-if="!visibleOptions.length" class="app-select-empty" role="status">{{ emptyText }}</p>
      </div>
    </Teleport>
  </div>
</template>

<script setup lang="ts">
import { ArrowDown, Check } from '@element-plus/icons-vue';
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useId, watch, type CSSProperties } from 'vue';

type SelectOption = { value: string; label: string; disabled?: boolean };
const props = defineProps<{
  modelValue: string;
  options: SelectOption[];
  label: string;
  placeholder?: string;
  disabled?: boolean;
  required?: boolean;
  compact?: boolean;
  searchable?: boolean;
  searchPlaceholder?: string;
  emptyText?: string;
}>();
const emit = defineEmits<{ 'update:modelValue': [value: string]; change: [value: string] }>();
const root = ref<HTMLElement | null>(null);
const trigger = ref<HTMLButtonElement | null>(null);
const menu = ref<HTMLElement | null>(null);
const menuStyle = ref<CSSProperties>({});
const open = ref(false);
const filter = ref('');
const searchInput = ref<HTMLInputElement | null>(null);
const visibleOptions = computed(() => props.searchable ? props.options.filter(option => `${option.label} ${option.value}`.toLocaleLowerCase().includes(filter.value.trim().toLocaleLowerCase())) : props.options);
const activeIndex = ref(-1);
const listId = useId();
const selectedOption = computed(() => props.options.find(option => option.value === props.modelValue));
let search = '';
let searchTimer: number | undefined;

function enabled(index: number): boolean { return index >= 0 && index < visibleOptions.value.length && !visibleOptions.value[index].disabled; }
function firstEnabled(): number { return visibleOptions.value.findIndex(option => !option.disabled); }
function lastEnabled(): number {
  for (let index = visibleOptions.value.length - 1; index >= 0; index--) if (!visibleOptions.value[index].disabled) return index;
  return -1;
}
function positionMenu(): void {
  if (!open.value || !trigger.value) return;
  const rect = trigger.value.getBoundingClientRect();
  const estimatedHeight = Math.min(menu.value?.scrollHeight || visibleOptions.value.length * 38 + 10, 228);
  const below = window.innerHeight - rect.bottom - 12;
  const above = rect.top - 12;
  const placeAbove = below < estimatedHeight && above > below;
  const maxHeight = Math.max(80, Math.min(228, (placeAbove ? above : below) - 6));
  menuStyle.value = {
    top: `${placeAbove ? Math.max(6, rect.top - Math.min(estimatedHeight, maxHeight) - 6) : rect.bottom + 6}px`,
    left: `${rect.left}px`,
    width: `${rect.width}px`,
    maxHeight: `${maxHeight}px`,
  };
}
function scrollToActive(): void {
  void nextTick(() => {
    menu.value?.querySelectorAll('.app-select-option')[activeIndex.value]?.scrollIntoView({ block: 'nearest' });
    positionMenu();
  });
}
function show(): void {
  if (props.disabled || !visibleOptions.value.length) return;
  filter.value = '';
  const selected = visibleOptions.value.findIndex(option => option.value === props.modelValue && !option.disabled);
  activeIndex.value = selected >= 0 ? selected : firstEnabled();
  open.value = true;
  void nextTick(() => { positionMenu(); scrollToActive(); if (props.searchable) searchInput.value?.focus(); });
}
function hide(): void { open.value = false; search = ''; filter.value = ''; }
function toggle(): void { if (open.value) hide(); else show(); }
function choose(index: number): void {
  if (!enabled(index)) return;
  const value = visibleOptions.value[index].value;
  if (value !== props.modelValue) {
    emit('update:modelValue', value);
    emit('change', value);
  }
  hide();
  trigger.value?.focus();
}
function move(step: number): void {
  if (!open.value) { show(); return; }
  let index = activeIndex.value;
  for (let count = 0; count < visibleOptions.value.length; count++) {
    index = (index + step + visibleOptions.value.length) % visibleOptions.value.length;
    if (enabled(index)) { activeIndex.value = index; scrollToActive(); return; }
  }
}
function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape' && open.value) { event.preventDefault(); event.stopPropagation(); hide(); trigger.value?.focus(); return; }
  if (event.key === 'Tab') { hide(); return; }
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); move(event.key === 'ArrowDown' ? 1 : -1); return; }
  if (event.key === 'Home' || event.key === 'End') {
    event.preventDefault(); if (!open.value) show(); activeIndex.value = event.key === 'Home' ? firstEnabled() : lastEnabled(); scrollToActive(); return;
  }
  if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault(); if (open.value) choose(activeIndex.value); else show(); return;
  }
  if (event.key.length !== 1 || event.ctrlKey || event.altKey || event.metaKey) return;
  search += event.key.toLocaleLowerCase();
  window.clearTimeout(searchTimer);
  searchTimer = window.setTimeout(() => { search = ''; }, 650);
  const match = visibleOptions.value.findIndex(option => !option.disabled && option.label.toLocaleLowerCase().startsWith(search));
  if (match >= 0) { if (!open.value) show(); activeIndex.value = match; scrollToActive(); }
}
function onSearchKeydown(event: KeyboardEvent): void {
  if (event.isComposing) return;
  if (['ArrowDown', 'ArrowUp', 'Enter', 'Escape', 'Tab'].includes(event.key)) onKeydown(event);
}
watch(filter, () => { activeIndex.value = firstEnabled(); scrollToActive(); });
function onBlur(event: FocusEvent): void {
  const next = event.relatedTarget as Node | null;
  if (!root.value?.contains(next) && !menu.value?.contains(next)) hide();
}
function onPointerDown(event: PointerEvent): void {
  const target = event.target as Node;
  if (!root.value?.contains(target) && !menu.value?.contains(target)) hide();
}
watch(() => props.disabled, value => { if (value) hide(); });
onMounted(() => {
  document.addEventListener('pointerdown', onPointerDown);
  window.addEventListener('scroll', positionMenu, true);
  window.addEventListener('resize', positionMenu);
});
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onPointerDown);
  window.removeEventListener('scroll', positionMenu, true);
  window.removeEventListener('resize', positionMenu);
  window.clearTimeout(searchTimer);
});
</script>

<style scoped>
.app-select { width: 100%; min-width: 0; }
.app-select.compact { width: 150px; }
.app-select-trigger { width: 100%; height: 42px; padding: 0 12px; display: flex; align-items: center; justify-content: space-between; gap: 12px; border: 1px solid #d8e0eb; border-radius: 7px; color: var(--ink); background: #fbfcfe; text-align: left; font-size: 14px; }
.app-select-trigger:hover:not(:disabled) { border-color: #a9cef8; }
.app-select-trigger:focus-visible, .app-select-trigger[aria-expanded="true"] { outline: 0; border-color: #68aaf4; box-shadow: 0 0 0 3px rgb(20 120 242 / 10%); background: #fff; }
.app-select-trigger:disabled { color: #8792a3; background: #f1f3f6; cursor: not-allowed; }
.app-select-trigger > span { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.app-select-trigger .placeholder { color: #929cad; }
.app-select-chevron { width: 14px; height: 14px; flex: none; color: #68758a; transition: transform .15s ease; }
.app-select-trigger[aria-expanded="true"] .app-select-chevron { transform: rotate(180deg); }
.compact .app-select-trigger { min-width: 150px; height: 34px; padding: 0 10px; color: #45546a; background: #fff; font-size: 13px; }
.app-select-menu { position: fixed; z-index: 120; padding: 5px; overflow-y: auto; background: var(--surface); border: 1px solid var(--line); border-radius: 8px; box-shadow: 0 12px 28px rgb(25 54 96 / 14%); }
.app-select-search { position: sticky; top: -5px; padding: 4px 0 8px; background: var(--surface); }
.app-select-search input { box-sizing: border-box; width: 100%; min-width: 0; padding: 7px 10px; border: 1px solid var(--line); border-radius: 5px; }
.app-select-empty { margin: 8px 10px; color: var(--muted); font-size: 13px; }
.app-select-option { width: 100%; min-height: 36px; padding: 7px 10px; display: flex; align-items: center; justify-content: space-between; gap: 8px; border: 0; border-radius: 5px; color: #455269; background: transparent; text-align: left; font-size: 14px; }
.app-select-option:hover:not(:disabled), .app-select-option.active { color: var(--blue); background: #edf6ff; }
.app-select-option.selected { color: var(--blue); font-weight: 650; }
.app-select-option:disabled { color: #9aa6b6; cursor: not-allowed; }
.app-select-option svg { width: 15px; height: 15px; flex: none; }
</style>
