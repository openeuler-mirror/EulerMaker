import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
import { parse, stringify } from 'yaml';
const compile = source => 'data:text/javascript;base64,' + Buffer.from(ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText).toString('base64');
const gitRef = compile(readFileSync(new URL('../src/utils/gitRef.ts', import.meta.url), 'utf8'));
const source = readFileSync(new URL('../src/utils/packageReposYaml.ts', import.meta.url), 'utf8').replace("'./gitRef'", JSON.stringify(gitRef));
const { validatePackageRepos } = await import(compile(source));
const read = source => validatePackageRepos(parse(source, { uniqueKeys: true }));
test('package YAML round-trips refs, commit pins and additional fields', () => {
  const repos = [{ name: 'gcc', url: 'https://example.com/gcc.git', ref: 'refs/heads/main', extra: { retained: true } }, { name: 'rpm', url: 'git@example.com:rpm.git', commitID: 'a'.repeat(40) }];
  assert.deepEqual(read(stringify(repos)), repos);
  assert.deepEqual(read('[]'), []);
  assert.deepEqual(read('- name: " gcc "\n  url: " https://example.com/gcc.git "'), [{ name: 'gcc', url: 'https://example.com/gcc.git' }]);
});
test('invalid YAML structures and duplicate entries cannot replace the repository list', () => {
  for (const source of ['', 'null', '{}', 'packageRepos: []', '- null', '- name: gcc', '- name: ""\n  url: x', '- name: 123\n  url: x', '- name: gcc\n  url: []', '- name: gcc\n  url: x\n- name: " gcc "\n  url: y', '- name: gcc\n  name: rpm\n  url: x']) assert.throws(() => read(source), undefined, source);
});
test('Git refs and commit pins use the same rules as the form', () => {
  for (const extra of [{ ref: 'master' }, { ref: 123 }, { ref: 'refs/heads/a..b' }, { commitID: 'abc' }, { commitID: 123 }, { commitID: 'A'.repeat(40) }]) assert.throws(() => validatePackageRepos([{ name: 'gcc', url: 'x', ...extra }]));
  assert.equal(validatePackageRepos([{ name: 'gcc', url: 'x', ref: '', commitID: '' }]).length, 1);
});
