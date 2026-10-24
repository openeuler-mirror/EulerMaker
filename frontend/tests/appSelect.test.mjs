import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
import { parse } from '@vue/compiler-sfc';
import { computed, ref, watch, nextTick } from 'vue';
const { descriptor } = parse(readFileSync(new URL('../src/components/AppSelect.vue', import.meta.url), 'utf8'));
const source = descriptor.scriptSetup.content.replace(/^import .*;\n/gm, '');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
function setup(searchable) {
  const events = [];
  const props = { modelValue: 'alpha', options: [{ value: 'alpha', label: 'Alpha' }, { value: 'beta', label: 'Beta' }], searchable };
  const create = new Function('defineProps', 'defineEmits', 'computed', 'ref', 'watch', 'nextTick', 'useId', 'onMounted', 'onBeforeUnmount', outputText + '\nreturn { filter, visibleOptions, selectedOption, choose, show, open, onSearchKeydown };');
  const state = create(() => props, () => (...args) => events.push(args), computed, ref, watch, nextTick, () => 'select-test', () => {}, () => {});
  return { ...state, events };
}
test('search filters options without losing the selected label and chooses the filtered value', async () => {
  const state = setup(true);
  state.show();
  state.filter.value = ' BET ';
  await nextTick();
  assert.deepEqual(state.visibleOptions.value.map(option => option.value), ['beta']);
  assert.equal(state.selectedOption.value.label, 'Alpha');
  state.onSearchKeydown({ key: 'Enter', preventDefault() {} });
  assert.deepEqual(state.events, [['update:modelValue', 'beta'], ['change', 'beta']]);
  assert.equal(state.open.value, false);
  assert.equal(state.filter.value, '');
});
test('empty search results cannot select a stale option', async () => {
  const state = setup(true);
  state.show();
  state.filter.value = 'missing';
  await nextTick();
  state.onSearchKeydown({ key: 'Enter', preventDefault() {} });
  assert.deepEqual(state.events, []);
  assert.deepEqual(state.visibleOptions.value, []);
});
test('non-searchable selects retain all options', () => {
  const state = setup(false);
  state.filter.value = 'beta';
  assert.equal(state.visibleOptions.value.length, 2);
});
