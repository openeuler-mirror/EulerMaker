import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { compile } from '@vue/compiler-dom';
import * as Vue from 'vue';
import { renderToString } from '@vue/server-renderer';

const source = readFileSync(new URL('../src/components/ProjectJobs.vue', import.meta.url), 'utf8');
const template = source.match(/<a\s[^>]*:download=[^>]*>[\s\S]*?<\/a>/)?.[0];
assert.ok(template);
const render = new Function('Vue', compile(template, { mode: 'function', prefixIdentifiers: true }).code)(Vue);
const messages = Object.fromEntries(['zh-CN', 'en-US'].map(locale => {
  const text = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), 'utf8');
  const jobLog = text.match(/jobLog: (\{[\s\S]*?\n  \}),/)[1];
  return [locale, { jobLog: new Function(`return (${jobLog});`)() }];
}));

test('job actions offer a full log download in both languages for running and finished jobs', async () => {
  for (const locale of ['zh-CN', 'en-US']) {
    for (const phase of ['Pending', 'Running', 'Succeeded', 'Failed', 'Aborted']) {
      const html = await renderToString(Vue.createSSRApp({
        setup: () => ({
          project: 'project #1', job: { metadata: { name: 'job+1' }, status: { phase } },
          canAbort: false,
          t: key => key.split('.').reduce((value, part) => value[part], messages[locale]),
        }),
        render,
      }));
      assert.ok(html.includes('href="/artifacts/v1/projects/project%20%231/jobs/job%2B1/logs/content"'));
      assert.ok(html.includes('download="job+1.log"'));
      assert.ok(html.includes(messages[locale].jobLog.download));
      assert.ok(html.includes(messages[locale].jobLog.downloadHint));
    }
  }
});

test('a job without a name has no log download link', async () => {
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ project: 'project', job: {}, t: key => key }), render,
  }));
  assert.equal(html.includes('<a '), false);
});
