import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
import { parse, stringify } from 'yaml';
import { parse as parseSFC } from '@vue/compiler-sfc';
import { ref } from 'vue';

const source = readFileSync(new URL('../src/components/serviceConfigDraft.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { parseServiceContent, buildServiceContent, validServiceURL } = await import(
  'data:text/javascript;base64,' + Buffer.from(outputText).toString('base64')
);

test('default service configuration round-trips both services', () => {
  const config = parse(readFileSync(new URL('../../backend/ebs-apiserver/pkg/server/default-service-config.yaml', import.meta.url), 'utf8'));
  const content = parseServiceContent(config);
  const rows = Object.entries(content.artifactManagers).map(([name, instance]) => ({ name, ...instance }));
  assert.deepEqual(parse(stringify(buildServiceContent(rows, content.gitServer.internalURL))), content);
  assert.equal(content.gitServer.internalURL, 'http://git-server:8080');
  assert.equal(content.artifactManagers.default.internalURL, 'http://artifact-manager:8081');
});

test('editing Git Server and artifact instances preserves both sections', () => {
  const content = parseServiceContent({ artifactManagers: { 'art-01': { internalURL: 'http://art-01:8081' } },
    gitServer: { internalURL: 'http://git:8080/prefix' } });
  const rows = Object.entries(content.artifactManagers).map(([name, instance]) => ({ name, ...instance }));
  rows[0].runnerExternalURL = 'http://artifacts.example:8081';
  rows.push({ name: 'art-02', internalURL: 'https://art-02/prefix', runnerInternalURL: '', runnerExternalURL: '' });
  const saved = buildServiceContent(rows, 'https://git.example/new-prefix');
  assert.deepEqual(Object.keys(saved.artifactManagers), ['art-01', 'art-02']);
  assert.equal(saved.artifactManagers['art-01'].runnerExternalURL, rows[0].runnerExternalURL);
  assert.equal(saved.gitServer.internalURL, 'https://git.example/new-prefix');
  assert.deepEqual(parseServiceContent(parse(stringify(saved))), saved);
});

test('form validation follows service-config instance and address rules', () => {
  const row = { name: 'art-01', internalURL: 'http://art:8081', runnerInternalURL: '', runnerExternalURL: '' };
  assert.throws(() => buildServiceContent([], 'http://git'), /invalidContent/);
  assert.throws(() => buildServiceContent([row, row], 'http://git'), /duplicateName/);
  for (const name of ['art.example', 'art_01', 'a'.repeat(64)]) {
    assert.throws(() => buildServiceContent([{ ...row, name }], 'http://git'), /invalidName/);
  }
  for (const url of ['', 'http://user:password@host', 'http://host/path?q=1', 'http://host/#frag',
    'http://host/white space', 'http://host:0', 'http://host:65536', 'http://host:']) {
    assert.equal(validServiceURL(url), false, url);
    assert.throws(() => buildServiceContent([row], url), /invalidURL/);
  }
  assert.equal(validServiceURL('http://host:65535/prefix'), true);
  assert.equal(validServiceURL('', true), true);
});

const component = readFileSync(new URL('../src/components/ServiceConfigEditor.vue', import.meta.url), 'utf8');
const script = ts.createSourceFile('ServiceConfigEditor.ts', parseSFC(component).descriptor.scriptSetup.content,
  ts.ScriptTarget.Latest, true);
const editorCode = ts.transpileModule(script.statements.filter(s => !ts.isImportDeclaration(s))
  .map(s => s.getText(script)).join('\n'), { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;

function editor() {
  const requests = [];
  let conflict = false;
  const original = { metadata: { name: 'service-config', uid: 'config-uid', resourceVersion: '1' },
    spec: { visibility: 'OpsOnly', content: stringify({ artifactManagers: { 'art-01': { internalURL: 'http://art:8081' } },
      gitServer: { internalURL: 'http://git:8080' } }) } };
  class ApiError extends Error { constructor(status) { super(); this.status = status; } }
  const dependencies = { ref, onMounted: () => {}, useI18n: () => ({ t: key => key }), parse, stringify,
    parseServiceContent, buildServiceContent, ApiError, errorTranslationKey: (_error, fallback) => fallback,
    window: { confirm: () => true },
    request: async (path, options = {}) => {
      requests.push({ path, options });
      if (options.method === 'PUT') {
        if (conflict) throw new ApiError(409);
        const saved = JSON.parse(options.body);
        saved.metadata.resourceVersion = '2';
        return saved;
      }
      return structuredClone(original);
    } };
  const state = new Function(...Object.keys(dependencies), editorCode +
    '\nreturn {load, save, startEdit, cancelEdit, addInstance, removeInstance, current, instances, gitServerURL, editing, error, success};'
  )(...Object.values(dependencies));
  return { state, requests, original, setConflict: () => { conflict = true; } };
}

test('editor loads and saves service-config with current metadata and both service sections', async () => {
  const { state, requests } = editor();
  await state.load();
  state.startEdit();
  state.gitServerURL.value = 'https://git.example/prefix';
  state.instances.value[0].runnerExternalURL = 'http://runner.example:8081';
  await state.save();
  assert.deepEqual(requests.map(r => r.path), ['/apis/ebs/v1/configs/service-config', '/apis/ebs/v1/configs/service-config']);
  const body = JSON.parse(requests[1].options.body);
  assert.equal(body.metadata.resourceVersion, '1');
  assert.equal(body.spec.visibility, 'OpsOnly');
  assert.equal(parse(body.spec.content).gitServer.internalURL, 'https://git.example/prefix');
  assert.equal(parse(body.spec.content).artifactManagers['art-01'].runnerExternalURL, 'http://runner.example:8081');
  assert.equal(state.current.value.metadata.resourceVersion, '2');
  assert.equal(state.editing.value, false);
  assert.equal(state.success.value, true);
});

test('cancel restores both service drafts without an update request', async () => {
  const { state, requests } = editor();
  await state.load();
  state.startEdit();
  state.gitServerURL.value = 'http://changed';
  state.removeInstance(0);
  state.addInstance();
  state.cancelEdit();
  assert.equal(state.gitServerURL.value, 'http://git:8080');
  assert.deepEqual(state.instances.value.map(i => i.name), ['art-01']);
  assert.equal(state.editing.value, false);
  assert.equal(requests.length, 1);
});

test('a conflicting update keeps the draft and asks the user to refresh', async () => {
  const { state, setConflict } = editor();
  await state.load();
  state.startEdit();
  state.gitServerURL.value = 'http://changed';
  setConflict();
  await state.save();
  assert.equal(state.error.value, 'serviceConfig.conflict');
  assert.equal(state.editing.value, true);
  assert.equal(state.gitServerURL.value, 'http://changed');
  assert.equal(state.current.value.metadata.resourceVersion, '1');
});
