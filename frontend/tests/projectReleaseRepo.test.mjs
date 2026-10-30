import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';
const source = readFileSync(new URL('../src/utils/projectReleaseRepo.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { projectReleaseRepoUrl, filterReleaseProjects } = await import('data:text/javascript;base64,' + Buffer.from(outputText).toString('base64'));
const project = (name, distro, instance) => ({ metadata: { name, annotations: instance ? { 'ebs.io/artifacts': instance } : {} }, spec: { distro, buildArches: ['x86_64'] } });
test('release URLs use the source instance and project without a distro or architecture suffix', () => {
  const source = project('source', 'openEuler', 'art-02');
  assert.equal(projectReleaseRepoUrl(source), 'artifact://art-02/repositories/source');
  delete source.metadata.annotations['ebs.io/artifacts'];
  assert.equal(projectReleaseRepoUrl(source), 'artifact://default/repositories/source');
});
test('filtering uses project names and display names across distributions', () => {
  const projects = [project('current', 'os-a'), project('base-b', 'os-b'), project('base-a', 'os-a')];
  projects[2].spec.displayName = 'Shared Base';
  const names = query => filterReleaseProjects(projects, 'current', query).map(p => p.metadata.name);
  assert.deepEqual(names(''), ['base-a', 'base-b']);
  assert.deepEqual(names(' BASE '), ['base-a', 'base-b']);
  assert.deepEqual(names('shared'), ['base-a']);
});
