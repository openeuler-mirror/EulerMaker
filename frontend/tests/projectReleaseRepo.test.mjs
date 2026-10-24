import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
const source = readFileSync(new URL('../src/utils/projectReleaseRepo.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { projectReleaseRepoUrl, releaseOperatingSystems, filterReleaseProjects } = await import('data:text/javascript;base64,' + Buffer.from(outputText).toString('base64'));
const project = (name, targets, instance) => ({ metadata: { name, annotations: instance ? { 'ebs.io/artifacts': instance } : {} }, spec: { buildTargets: targets.map(([os, arch]) => ({ os, arch })) } });
test('release URLs use the source instance and encode OS without an architecture suffix', () => {
  const source = project('source', [['openEuler:24/preview', 'aarch64']], 'art-02');
  assert.equal(projectReleaseRepoUrl(source, 'openEuler:24/preview'), 'artifact://art-02/repositories/source/openEuler%3A24%2Fpreview');
  delete source.metadata.annotations['ebs.io/artifacts'];
  assert.equal(projectReleaseRepoUrl(source, 'openEuler:24/preview'), 'artifact://default/repositories/source/openEuler%3A24%2Fpreview');
  assert.equal(projectReleaseRepoUrl(source, 'unknown-os'), '');
  assert.equal(projectReleaseRepoUrl({}, 'os'), '');
});
test('OS options deduplicate architectures and ignore missing OS values', () => {
  assert.deepEqual(releaseOperatingSystems(project('source', [['os-b', 'aarch64'], ['os-a', 'x86_64'], ['os-a', 'aarch64'], ['', 'x86_64']])), ['os-a', 'os-b']);
});
test('filtering combines project name and OS and excludes the current project', () => {
  const projects = [project('current', [['os-a', 'aarch64']]), project('base-b', [['os-b', 'x86_64']]), project('base-a', [['os-a', 'x86_64']]), project('empty', [])];
  projects[2].spec.displayName = 'Shared Base';
  const names = (query, os) => filterReleaseProjects(projects, 'current', query, os).map(p => p.metadata.name);
  assert.deepEqual(names('', ''), ['base-a', 'base-b']);
  assert.deepEqual(names(' BASE ', 'os-a'), ['base-a']);
  assert.deepEqual(names('shared', ''), ['base-a']);
  assert.deepEqual(names('base-b', 'os-a'), []);
});
