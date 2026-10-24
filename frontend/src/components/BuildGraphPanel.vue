<template>
  <article class="content-panel build-graph-panel">
    <div class="section-heading">
      <div><h2>{{ t('project.graphTab') }}</h2><p>{{ t('graph.hint') }}</p></div>
      <button class="secondary-button compact-button" type="button" :disabled="loading || buildsLoading"
        :title="t('common.refresh')" :aria-label="t('common.refresh')" @click="refresh"><Refresh aria-hidden="true" /></button>
    </div>
    <div class="graph-build-toolbar">
      <AppSelect :model-value="buildName" :options="buildOptions" :label="t('graph.selectBuild')"
        :placeholder="t('graph.selectBuild')" :disabled="buildsLoading" @change="selectBuild" />
      <div class="pagination-row graph-pager">
        <button class="page-button arrow-button" type="button" :disabled="buildsLoading || buildPage === 0"
          :aria-label="t('common.previous')" @click="loadBuildPage(buildPage - 1)"><ArrowLeft /></button>
        <span>{{ t('graph.buildPage', { page: buildPage + 1 }) }}</span>
        <button class="page-button arrow-button" type="button" :disabled="buildsLoading || !nextToken"
          :aria-label="t('common.next')" @click="loadBuildPage(buildPage + 1)"><ArrowRight /></button>
      </div>
    </div>
    <p v-if="buildsError" class="inline-error" role="alert">{{ t(buildsError) }}</p>
    <p v-if="loading || buildsLoading && !buildName" class="config-empty" role="status">{{ t('graph.loading') }}</p>
    <p v-else-if="errorKey" class="inline-error" role="alert">{{ t(errorKey) }}</p>
    <p v-else-if="missing" class="config-empty">{{ t('graph.missing') }}</p>
    <p v-else-if="!buildName && !buildsError" class="config-empty">{{ t('graph.noBuilds') }}</p>
    <p v-else-if="graph && !index.names.length" class="config-empty">{{ t('graph.empty') }}</p>
    <template v-else-if="graph">
      <div class="graph-summary">
        <span>{{ t('graph.summary', { nodes: index.names.length, edges: index.edges.length }) }}</span>
        <span class="graph-legend build">{{ t('graph.buildEdge') }}</span>
        <span class="graph-legend install">{{ t('graph.installEdge') }}</span>
        <span>{{ t('graph.breakLegend') }}</span>
      </div>
      <div class="graph-layout">
        <aside class="graph-sidebar">
          <label class="search-box"><Search /><input v-model="search" type="search"
            :placeholder="t('project.searchSpecs')" :aria-label="t('project.searchSpecs')" /></label>
          <p class="graph-search-count">{{ t('common.count', { count: filteredNames.length }) }}</p>
          <ul class="graph-node-list">
            <li v-for="nodeName in visibleNames" :key="nodeName">
              <button type="button" :class="{ active: selectedNode === nodeName }" :title="nodeName"
                :aria-pressed="selectedNode === nodeName" @click="focusNode(nodeName)">
                {{ nodes[nodeName].bootstrapBreak ? '★ ' : '' }}{{ nodeName }}
              </button>
            </li>
          </ul>
          <p v-if="!filteredNames.length" class="config-empty">{{ t('project.noMatchingSpecs') }}</p>
          <div v-if="filteredNames.length" class="pagination-row graph-pager">
            <button class="page-button arrow-button" type="button" :disabled="nodePage === 0"
              :aria-label="t('common.previous')" @click="nodePage--"><ArrowLeft /></button>
            <span>{{ nodePage + 1 }} / {{ nodePages }}</span>
            <button class="page-button arrow-button" type="button" :disabled="nodePage + 1 >= nodePages"
              :aria-label="t('common.next')" @click="nodePage++"><ArrowRight /></button>
          </div>
        </aside>
        <section class="graph-detail" :aria-label="t('project.graphTab')">
          <div class="graph-focus-heading">
            <strong>{{ selectedNode }}</strong>
            <code v-if="nodes[selectedNode]?.version">{{ nodes[selectedNode].version }}</code>
            <span v-if="nodes[selectedNode]?.bootstrapBreak" class="status-badge">{{ t('graph.breakPoint') }}</span>
          </div>
          <p class="graph-help">{{ t('graph.direction') }}</p>
          <div class="graph-column-headings">
            <div v-for="side in sides" :key="side.id">
              <strong>{{ t(side.id === 'upstream' ? 'graph.upstream' : 'graph.downstream', { count: side.names.length }) }}</strong>
            </div>
          </div>
          <div class="graph-viewer" @keydown="handleFindShortcut">
            <div class="graph-findbar">
              <label class="search-box">
                <Search />
                <input ref="findInput" v-model="findQuery" type="search"
                  :placeholder="t('graph.findNodes')" :aria-label="t('graph.findNodes')"
                  @keydown.enter.prevent="moveMatch($event.shiftKey ? -1 : 1)"
                  @keydown.esc.prevent="findQuery = ''" />
              </label>
              <span class="graph-find-count" role="status">{{ findMatches.length ? matchIndex + 1 : 0 }} / {{ findMatches.length }}</span>
              <div class="pagination-row">
                <button class="page-button arrow-button" type="button" :disabled="!findMatches.length"
                  :title="t('common.previous')" :aria-label="t('common.previous')" @click="moveMatch(-1)"><ArrowUp /></button>
                <button class="page-button arrow-button" type="button" :disabled="!findMatches.length"
                  :title="t('common.next')" :aria-label="t('common.next')" @click="moveMatch(1)"><ArrowDown /></button>
                <button class="page-button arrow-button" type="button" :disabled="!findQuery"
                  :title="t('graph.clearFind')" :aria-label="t('graph.clearFind')" @click="findQuery = ''"><Close /></button>
              </div>
            </div>
            <div ref="canvas" class="graph-canvas">
            <p class="graph-navigation-hint">{{ t('graph.navigationHint') }}</p>
            <svg :viewBox="`0 0 ${layout.width} ${layout.height}`"
              :style="{ width: `${layout.width}px`, height: `${layout.height}px` }"
              role="group" :aria-label="t('graph.neighborhood', { name: selectedNode })">
              <defs>
                <marker v-for="kind in ['build', 'install']" :id="`${markerId}-${kind}`" :key="kind"
                  viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
                  <path d="M 0 0 L 10 5 L 0 10 z" :fill="kind === 'build' ? '#1478f2' : '#a56a23'" />
                </marker>
              </defs>
              <path v-for="edge in visibleEdges" :key="`${edge.source}/${edge.target}/${edge.kind}`"
                :d="edgePath(edge)" :class="['graph-edge', edge.kind]" :marker-end="`url(#${markerId}-${edge.kind})`">
                <title>{{ edge.source }} → {{ edge.target }} · {{ t(`graph.${edge.kind}Edge`) }} {{ constraintLabel(edge.constraint) }}</title>
              </path>
              <foreignObject v-for="side in sides" :key="`${selectedNode}/${side.id}`"
                :x="side.id === 'upstream' ? 20 : 740" y="20" width="260" :height="layout.height - 40">
                <div class="graph-neighbor-scroll" tabindex="0" :data-side="side.id"
                  :style="{ paddingTop: `${side.id === 'upstream' ? layout.topPadding.upstream : layout.topPadding.downstream}px` }"
                  :aria-label="t(side.id === 'upstream' ? 'graph.upstream' : 'graph.downstream', { count: side.names.length })"
                  @scroll="scrollNeighbors(side.id, $event)">
                  <button v-for="name in side.names" :key="name" type="button" class="graph-neighbor"
                    :class="matchClasses(side.id, name)" :title="name" @click="focusNode(name)">
                    {{ nodes[name]?.bootstrapBreak ? '★ ' : '' }}{{ name }}
                  </button>
                </div>
              </foreignObject>
              <g v-for="node in layout.positions.slice(0, 1)" :key="`${node.column}-${node.name}`" :transform="`translate(${node.x},${node.y})`"
                :class="['graph-node', { focused: node.column === 'center' }, matchClasses('center', node.name)]" role="button" tabindex="0"
                :aria-label="node.name" @click="focusNode(node.name)" @keydown.enter.prevent="focusNode(node.name)"
                @keydown.space.prevent="focusNode(node.name)">
                <title>{{ node.name }}{{ nodes[node.name]?.bootstrapBreak ? ` · ${t('graph.breakPoint')}` : '' }}</title>
                <rect width="240" height="44" rx="8" />
                <text x="120" y="27" text-anchor="middle">
                  {{ nodes[node.name]?.bootstrapBreak ? '★ ' : '' }}{{ shorten(node.name) }}
                </text>
              </g>
            </svg>
            </div>
          </div>
          <p v-if="selfEdges.length" class="graph-help">{{ t('graph.selfDependency') }}
            <span v-for="edge in selfEdges" :key="edge.kind">{{ t(`graph.${edge.kind}Edge`) }} {{ constraintLabel(edge.constraint) }} </span>
          </p>
          <p v-if="!index.upstream.get(selectedNode)?.length && !index.downstream.get(selectedNode)?.length"
            class="config-empty">{{ t('graph.isolated') }}</p>
        </section>
      </div>
    </template>
  </article>
</template>

<script setup lang="ts">
import { ArrowDown, ArrowLeft, ArrowRight, ArrowUp, Close, Refresh, Search } from '@element-plus/icons-vue';
import { computed, onBeforeUnmount, ref, shallowRef, useId, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import { ApiError, errorTranslationKey, request } from '@/api';
import type { Build, BuildGraph, ResourceList } from '@/types';
import { constraintLabel, dependencyPath, filterGraphNames, findNeighborhoodMatches, indexBuildGraph, isVisibleNeighbor, layoutNeighborhood, type DependencyEdge } from '@/utils/buildGraph';
import AppSelect from './AppSelect.vue';

const props = defineProps<{ project: string; initialBuildName?: string }>();
const { t } = useI18n();
const builds = shallowRef<Build[]>([]);
const buildName = ref('');
const buildPage = ref(0);
const tokens = ref(['']);
const nextToken = ref('');
const buildsLoading = ref(false);
const buildsError = ref('');
const graph = shallowRef<BuildGraph | null>(null);
const loading = ref(false);
const missing = ref(false);
const errorKey = ref('');
const selectedNode = ref('');
const canvas = ref<HTMLDivElement>();
const neighborScroll = ref({ upstream: 0, downstream: 0 });
const findQuery = ref('');
const findInput = ref<HTMLInputElement>();
const matchIndex = ref(0);
const search = ref('');
const nodePage = ref(0);
const nodePageSize = 20;
const markerId = useId();
let buildRequest: AbortController | undefined;
let graphRequest: AbortController | undefined;
const nodes = computed(() => graph.value?.status.nodes || {});
const index = computed(() => indexBuildGraph(nodes.value));
const filteredNames = computed(() => filterGraphNames(index.value.names, search.value));
const nodePages = computed(() => Math.max(1, Math.ceil(filteredNames.value.length / nodePageSize)));
const visibleNames = computed(() => filteredNames.value.slice(nodePage.value * nodePageSize, (nodePage.value + 1) * nodePageSize));
const buildOptions = computed(() => builds.value.map(build => {
  const type = build.spec?.buildType || '';
  const typeKeys: Record<string, string> = {
    full: 'project.fullBuild',
    incremental: 'project.incrementalBuild',
    specified: 'project.specifiedBuild',
  };
  const typeLabel = typeKeys[type] ? t(typeKeys[type]) : type || t('project.unspecifiedType');
  return {
    value: build.metadata?.name || '',
    label: `${typeLabel} · ${build.spec?.buildTarget?.os || '—'} / ${build.spec?.buildTarget?.arch || '—'} · ${build.metadata?.name}`,
  };
}));
const sides = computed(() => [
  { id: 'upstream' as const, edges: index.value.upstream.get(selectedNode.value) || [] },
  { id: 'downstream' as const, edges: index.value.downstream.get(selectedNode.value) || [] },
].map(side => ({
  ...side,
  names: [...new Set(side.edges.map(edge => side.id === 'upstream' ? edge.source : edge.target))]
    .filter(name => name !== selectedNode.value).sort((a, b) => a.localeCompare(b)),
})));
const findMatches = computed(() => findNeighborhoodMatches(selectedNode.value, sides.value[0].names,
  sides.value[1].names, findQuery.value));
const matchKeys = computed(() => new Set(findMatches.value.map(node => `${node.column}/${node.name}`)));
const activeMatch = computed(() => findMatches.value[matchIndex.value]);
const layout = computed(() => layoutNeighborhood(selectedNode.value, sides.value[0].names, sides.value[1].names,
  neighborScroll.value));
const positionIndex = computed(() => new Map(layout.value.positions.map(node => [`${node.column}/${node.name}`, node])));
const selfEdges = computed(() => (index.value.upstream.get(selectedNode.value) || []).filter(edge => edge.source === edge.target));
const visibleEdges = computed(() => sides.value.flatMap(side => side.edges.filter(edge => {
  if (edge.source === edge.target) return false;
  const name = side.id === 'upstream' ? edge.source : edge.target;
  const position = positionIndex.value.get(`${side.id}/${name}`);
  return position !== undefined && isVisibleNeighbor(position, layout.value.height);
})));

function scrollNeighbors(side: string, event: Event): void {
  const scrollTop = (event.currentTarget as HTMLDivElement).scrollTop;
  if (side === 'upstream') neighborScroll.value.upstream = scrollTop;
  else neighborScroll.value.downstream = scrollTop;
}

function matchClasses(column: string, name: string) {
  return {
    'find-match': matchKeys.value.has(`${column}/${name}`),
    'find-current': activeMatch.value?.column === column && activeMatch.value.name === name,
  };
}

function moveMatch(delta: number): void {
  if (!findMatches.value.length) return;
  matchIndex.value = (matchIndex.value + delta + findMatches.value.length) % findMatches.value.length;
  revealMatch();
}

function revealMatch(): void {
  const match = activeMatch.value;
  const area = canvas.value;
  if (!match || !area) return;
  if (match.column !== 'center') {
    const list = area.querySelector<HTMLDivElement>(`[data-side="${match.column}"]`);
    if (list) {
      list.scrollTop = layout.value.topPadding[match.column] + match.index * 64 + 22 - list.clientHeight / 2;
      neighborScroll.value[match.column] = list.scrollTop;
    }
  }
  const node = positionIndex.value.get(`${match.column}/${match.name}`)!;
  const margin = Math.max(0, (area.clientWidth - layout.value.width) / 2);
  area.scrollLeft = Math.max(0, margin + node.x + 120 - area.clientWidth / 2);
  area.scrollTop = Math.max(0, node.y + 22 - area.clientHeight / 2);
}

function handleFindShortcut(event: KeyboardEvent): void {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'f') {
    event.preventDefault();
    findInput.value?.focus();
    findInput.value?.select();
  }
}

function edgePath(edge: DependencyEdge): string {
  const sourceColumn = edge.source === selectedNode.value ? 'center' : 'upstream';
  const targetColumn = edge.target === selectedNode.value ? 'center' : 'downstream';
  const source = positionIndex.value.get(`${sourceColumn}/${edge.source}`)!;
  const target = positionIndex.value.get(`${targetColumn}/${edge.target}`)!;
  return dependencyPath(source, target, edge.kind);
}

function shorten(name: string): string { return name.length > 26 ? `${name.slice(0, 24)}…` : name; }
function focusNode(name: string): void {
  selectedNode.value = name;
}
function selectBuild(name: string): void {
  buildName.value = name;
  selectedNode.value = search.value = '';
  nodePage.value = 0;
  void loadGraph();
}

async function loadBuildPage(page: number): Promise<void> {
  buildRequest?.abort();
  const current = new AbortController();
  buildRequest = current;
  buildsLoading.value = true;
  buildsError.value = '';
  const query = new URLSearchParams({
    limit: '50',
    labelSelector: 'ebs.io/build-type!=single',
    includeFields: 'metadata.name,spec.buildType,spec.buildTarget',
  });
  if (tokens.value[page]) query.set('continue', tokens.value[page]);
  try {
    const result = await request<ResourceList<Build>>(
      `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/builds?${query}`, { signal: current.signal });
    if (current.signal.aborted) return;
    builds.value = result.items || [];
    buildPage.value = page;
    nextToken.value = result.metadata?.continue || '';
    tokens.value = tokens.value.slice(0, page + 1);
    if (nextToken.value) tokens.value[page + 1] = nextToken.value;
    if (!buildName.value) selectBuild(props.initialBuildName || builds.value[0]?.metadata?.name || '');
  } catch (error) {
    if (!current.signal.aborted) buildsError.value = errorTranslationKey(error, 'errors.loadBuilds');
  } finally {
    if (!current.signal.aborted) buildsLoading.value = false;
  }
}

async function loadGraph(): Promise<void> {
  graphRequest?.abort();
  const current = new AbortController();
  graphRequest = current;
  graph.value = null;
  errorKey.value = '';
  missing.value = false;
  loading.value = !!buildName.value;
  if (!buildName.value) return;
  try {
    const result = await request<BuildGraph>(
      `/apis/ebs/v1/projects/${encodeURIComponent(props.project)}/buildgraphs/${encodeURIComponent(buildName.value)}`,
      { signal: current.signal });
    if (current.signal.aborted) return;
    graph.value = result;
    if (!Object.hasOwn(result.status.nodes, selectedNode.value)) {
      focusNode(Object.keys(result.status.nodes).sort((a, b) => a.localeCompare(b))[0] || '');
    }
    nodePage.value = 0;
  } catch (error) {
    if (current.signal.aborted) return;
    missing.value = error instanceof ApiError && error.status === 404;
    if (!missing.value) errorKey.value = errorTranslationKey(error, 'graph.loadFailed');
  } finally {
    if (!current.signal.aborted) loading.value = false;
  }
}

function refresh(): void {
  if (buildName.value) void loadGraph();
  void loadBuildPage(0);
}
watch(search, () => { nodePage.value = 0; });
watch(selectedNode, () => {
  findQuery.value = '';
  neighborScroll.value = { upstream: 0, downstream: 0 };
}, { flush: 'sync' });
watch(findMatches, () => {
  matchIndex.value = 0;
  revealMatch();
}, { flush: 'post' });
watch([selectedNode, canvas], () => {
  if (!canvas.value) return;
  const center = layout.value.positions[0];
  canvas.value.scrollLeft = Math.max(0, center.x + 120 - canvas.value.clientWidth / 2);
  canvas.value.scrollTop = Math.max(0, center.y + 22 - canvas.value.clientHeight / 2);
}, { flush: 'post' });
watch(() => props.project, () => {
  graphRequest?.abort();
  buildName.value = selectedNode.value = search.value = '';
  graph.value = null;
  errorKey.value = '';
  missing.value = loading.value = false;
  builds.value = [];
  tokens.value = [''];
  nextToken.value = '';
  buildPage.value = nodePage.value = 0;
  void loadBuildPage(0);
}, { immediate: true });
onBeforeUnmount(() => { buildRequest?.abort(); graphRequest?.abort(); });
</script>

<style scoped>
.graph-build-toolbar, .graph-summary, .graph-pager, .graph-focus-heading { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.graph-build-toolbar { margin: 18px 0; }
.graph-build-toolbar :deep(.app-select) { flex: 1; min-width: 240px; max-width: 800px; }
.graph-pager { gap: 8px; color: var(--muted); font-size: 12px; }
.graph-summary { padding: 12px 0; color: var(--muted); font-size: 13px; border-bottom: 1px solid var(--line); }
.graph-legend::before { content: ''; display: inline-block; width: 24px; margin-right: 6px; vertical-align: middle; border-top: 2px solid var(--blue); }
.graph-legend.install::before { border-top: 2px dashed #a56a23; }
.graph-layout { display: grid; grid-template-columns: 240px minmax(0, 1fr); gap: 20px; margin-top: 20px; }
.graph-sidebar { border-right: 1px solid var(--line); padding-right: 16px; }
.graph-sidebar .search-box { width: 100%; }
.graph-search-count, .graph-help { color: var(--muted); font-size: 12px; line-height: 1.7; }
.graph-node-list { list-style: none; padding: 0; margin: 12px 0; }
.graph-node-list button { width: 100%; text-align: left; padding: 9px; border: 0; background: transparent; border-radius: 6px; color: var(--ink); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.graph-node-list button:hover, .graph-node-list button.active { background: #edf5ff; color: var(--blue-deep); }
.graph-detail { min-width: 0; }
.graph-focus-heading { overflow-wrap: anywhere; }
.graph-focus-heading code { color: var(--muted); }
.graph-column-headings { display: flex; justify-content: flex-start; flex-wrap: wrap; gap: 20px; font-size: 13px; padding: 12px 0; }
.graph-column-headings .graph-pager { margin-top: 8px; }
.graph-viewer { background: #f8faff; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.graph-findbar { display: flex; justify-content: flex-end; align-items: center; flex-wrap: wrap; gap: 8px; padding: 10px 12px; border-bottom: 1px solid var(--line); }
.graph-findbar .search-box { width: min(260px, 100%); background: white; }
.graph-find-count { color: var(--muted); font-size: 12px; white-space: nowrap; }
.graph-canvas { position: relative; overflow: auto; max-height: 960px; }
.graph-canvas svg { display: block; margin-inline: auto; }
.graph-navigation-hint { position: absolute; top: 0; right: 0; z-index: 1; margin: 0; padding: 10px 12px; color: var(--muted); font-size: 12px; line-height: 1.7; text-align: right; pointer-events: none; }
.graph-neighbor-scroll { box-sizing: border-box; height: 100%; overflow-y: auto; overflow-x: hidden; display: flex; flex-direction: column; gap: 20px; overscroll-behavior-y: contain; }
.graph-neighbor { box-sizing: border-box; width: 240px; height: 44px; flex: 0 0 44px; padding: 0 12px; border: 1px solid #cbd8ec; border-radius: 8px; background: white; color: var(--ink); font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.graph-neighbor:hover, .graph-neighbor:focus-visible { background: #edf5ff; border-color: var(--blue); }
.graph-edge { fill: none; stroke: var(--blue); stroke-width: 1.8; }
.graph-edge.install { stroke: #a56a23; stroke-dasharray: 6 4; }
.graph-node { cursor: pointer; }
.graph-node rect { fill: white; stroke: #cbd8ec; }
.graph-node text { fill: var(--ink); font-size: 13px; }
.graph-node.focused rect, .graph-node:hover rect, .graph-node:focus rect { fill: #edf5ff; stroke: var(--blue); stroke-width: 2; }
.graph-neighbor.find-match { background: #fff3bf; border-color: #d3a527; }
.graph-node.find-match rect { fill: #fff3bf; stroke: #d3a527; }
.graph-neighbor.find-current { background: #ffda85; border: 2px solid #b86b09; }
.graph-node.find-current rect { fill: #ffda85; stroke: #b86b09; stroke-width: 2; }
@media (max-width: 850px) {
  .graph-layout { grid-template-columns: minmax(0, 1fr); }
  .graph-sidebar { border-right: 0; padding-right: 0; }
  .graph-node-list { max-height: 180px; overflow-y: auto; }
}
</style>
