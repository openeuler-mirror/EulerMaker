import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
import { parse } from '@vue/compiler-sfc';
import { computed, effectScope, nextTick, reactive, ref, watch } from 'vue';

const component = readFileSync(new URL('../src/components/PackageJobArtifacts.vue', import.meta.url), 'utf8');
const script = ts.createSourceFile('PackageJobArtifacts.ts', parse(component).descriptor.scriptSetup.content,
  ts.ScriptTarget.Latest, true);
const code = ts.transpileModule(script.statements.filter(s => !ts.isImportDeclaration(s))
  .map(s => s.getText(script)).join('\n'), { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;

const artifact = id => ({ id, fileName: `${id}.rpm`, category: 'artifact', state: 'Completed' });
const settle = async () => { await Promise.resolve(); await nextTick(); };

function setup(t, initial = {}) {
  const props = reactive({ project: 'project-a', jobName: 'job-a', succeeded: false, ...initial });
  const requests = [], mounted = [], unmounted = [];
  const scope = effectScope();
  const dependencies = {
    computed, ref, watch, defineProps: () => props, useI18n: () => ({ t: key => key }),
    onMounted: fn => mounted.push(fn), onBeforeUnmount: fn => unmounted.push(fn),
    errorTranslationKey: (_error, fallback) => fallback,
    request: (path, options) => new Promise((resolve, reject) => requests.push({ path, options, resolve, reject })),
  };
  const state = scope.run(() => new Function(...Object.keys(dependencies), code +
    '\nreturn {rpms, loading, errorKey, load};')(...Object.values(dependencies)));
  mounted.forEach(fn => fn());
  const unmount = () => { unmounted.forEach(fn => fn()); scope.stop(); };
  t.after(unmount);
  return { props, requests, state, unmount };
}

test('an expanded running job loads RPMs when refreshed to Succeeded', async t => {
  const { props, requests, state } = setup(t);
  assert.equal(requests.length, 0);
  props.succeeded = true;
  await nextTick();
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, '/artifacts/v1/projects/project-a/jobs/job-a/artifacts?category=artifact');
  requests[0].resolve({ items: [artifact('built')] });
  await settle();
  assert.deepEqual(state.rpms.value, [artifact('built')]);
  assert.equal(state.loading.value, false);
});

test('initially successful jobs keep filtering and sorting completed RPMs', async t => {
  const { requests, state } = setup(t, { succeeded: true });
  assert.equal(requests.length, 1);
  requests[0].resolve({ items: [artifact('z'), artifact('a'), { ...artifact('pending'), state: 'Uploading' },
    { ...artifact('log'), category: 'log' }, { ...artifact('text'), fileName: 'output.txt' }] });
  await settle();
  assert.deepEqual(state.rpms.value.map(item => item.id), ['a', 'z']);
});

test('changing job identity clears RPMs and ignores an older response', async t => {
  const { props, requests, state } = setup(t, { succeeded: true });
  requests[0].resolve({ items: [artifact('old')] });
  await settle();
  const retry = state.load();
  props.project = 'project-b';
  props.jobName = 'job-b';
  await nextTick();
  assert.equal(requests.length, 3);
  assert.equal(requests[1].options.signal.aborted, true);
  assert.deepEqual(state.rpms.value, []);
  assert.equal(requests[2].path, '/artifacts/v1/projects/project-b/jobs/job-b/artifacts?category=artifact');
  requests[1].resolve({ items: [artifact('stale')] });
  await retry;
  assert.deepEqual(state.rpms.value, []);
  assert.equal(state.loading.value, true);
  requests[2].resolve({ items: [artifact('new')] });
  await settle();
  assert.deepEqual(state.rpms.value, [artifact('new')]);
});

test('a non-successful replacement cancels the request without loading artifacts', async t => {
  const { props, requests, state } = setup(t, { succeeded: true });
  props.succeeded = false;
  props.jobName = 'running-job';
  await nextTick();
  assert.equal(requests.length, 1);
  assert.equal(requests[0].options.signal.aborted, true);
  requests[0].reject(new Error('late failure'));
  await settle();
  assert.deepEqual(state.rpms.value, []);
  assert.equal(state.errorKey.value, '');
  assert.equal(state.loading.value, false);
});

test('a response arriving after unmount cannot populate the artifact list', async t => {
  const { requests, state, unmount } = setup(t, { succeeded: true });
  unmount();
  requests[0].resolve({ items: [artifact('late')] });
  await settle();
  assert.equal(requests[0].options.signal.aborted, true);
  assert.deepEqual(state.rpms.value, []);
});
