<template>
  <div class="detail-toolbar">
    <RouterLink class="back-link" to="/projects"><ArrowLeft /> {{ t("project.back") }}</RouterLink>
    <div class="detail-actions">
      <button v-if="project" class="secondary-button" type="button" @click="exportYaml"><Download />{{ t("project.exportYaml") }}</button>
    </div>
  </div>
  <div v-if="exportErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(exportErrorKey) }}</div>
  <div v-if="copyErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(copyErrorKey) }}</div>
  <div v-if="buildActionErrorKey" class="inline-error action-error" role="alert"><WarningFilled />{{ t(buildActionErrorKey) }}</div>
  <div v-if="createdBuildNames.length" class="success-banner" role="status"><CircleCheckFilled />{{ createdBuildNames.length === 1 ? t("project.buildCreated", { name: createdBuildNames[0] }) : t("project.buildsCreated", { count: createdBuildNames.length }) }}</div>
  <div v-if="abortedBuildName" class="success-banner" role="status"><CircleCheckFilled />{{ t("project.buildAborted", { name: abortedBuildName }) }}</div>

  <div v-if="loadingProject" class="skeleton-list page-skeleton" :aria-label="t('project.loading')"><span v-for="item in 5" :key="item"></span></div>
  <div v-else-if="projectError" class="inline-error page-error">
    <WarningFilled /><span>{{ projectError }}</span><button type="button" @click="loadProject">{{ t("common.reload") }}</button>
  </div>
  <template v-else-if="project">
    <section class="project-hero">
      <div class="project-hero-copy">
        <div class="title-with-status"><h1>{{ project.spec?.displayName || project.metadata?.name }}</h1><span class="project-id"><code>{{ project.metadata?.name }}</code><button type="button" :class="{ copied: copiedId }" :aria-label="copiedId ? t('project.copied') : t('project.copyProjectId')" :title="copiedId ? t('project.copied') : t('project.copyProjectId')" @click="copyProjectId"><Check v-if="copiedId" /><DocumentCopy v-else /></button></span></div>
        <p>{{ project.spec?.description || t("project.noDescription") }}</p>
        <div v-if="canStartBuild" class="project-build-actions">
          <button class="primary-button" type="button" :disabled="buildConfigurationMissing" :title="buildConfigurationMissing ? t('project.buildConfigurationRequired') : ''" @click="openBuildDialog('full')">{{ t("project.fullBuild") }}</button>
          <button class="secondary-button" type="button" :disabled="buildConfigurationMissing" :title="buildConfigurationMissing ? t('project.buildConfigurationRequired') : ''" @click="openBuildDialog('incremental')">{{ t("project.incrementalBuild") }}</button>
          <button class="secondary-button" type="button" :disabled="buildConfigurationMissing" :title="buildConfigurationMissing ? t('project.buildConfigurationRequired') : ''" @click="openBuildDialog('specified')">{{ t("project.specifiedBuild") }}</button>
          <button class="secondary-button" type="button" :disabled="buildConfigurationMissing" :title="buildConfigurationMissing ? t('project.buildConfigurationRequired') : ''" @click="openBuildDialog('single')">{{ t("project.singleBuild") }}</button>
        </div>
      </div>
    </section>

    <nav class="project-tabs" role="tablist" :aria-label="t('project.tabsLabel')">
      <button v-for="tab in tabs" :id="`project-tab-${tab.id}`" :key="tab.id" type="button" role="tab" :aria-selected="activeTab === tab.id" :aria-controls="`project-panel-${tab.id}`" :class="{ active: activeTab === tab.id }" @click="selectTab(tab.id)">{{ tab.label }}</button>
    </nav>

    <section v-if="activeTab === 'overview'" id="project-panel-overview" class="project-tab-panel" role="tabpanel" aria-labelledby="project-tab-overview">
      <section class="metrics compact resource-metrics" :aria-label="t('project.overview')">
        <article class="metric-card"><div class="metric-icon violet"><Operation /></div><div><span>{{ t("project.builds") }}</span><strong>{{ resourceValue(builds) }}</strong><small>{{ t("project.buildsHint") }}</small></div></article>
        <article class="metric-card"><div class="metric-icon green"><Tickets /></div><div><span>{{ t("project.jobs") }}</span><strong>{{ resourceValue(jobs) }}</strong><small>{{ t("project.jobsHint") }}</small></div></article>
      </section>

      <ProjectJobs :project="project.metadata?.name || ''" :can-abort="canAbortJobs" />
      <section class="detail-grid">
        <article class="content-panel">
          <div class="section-heading"><div><h2>{{ t("project.info") }}</h2></div></div>
          <dl class="detail-list">
            <div><dt>{{ t("project.projectName") }}</dt><dd>{{ project.metadata?.name }}</dd></div>
            <div><dt>{{ t("project.displayName") }}</dt><dd>{{ project.spec?.displayName || t("common.emptyValue") }}</dd></div>
            <div><dt>{{ t("project.defaultRef") }}</dt><dd>{{ gitRefLabel(project.spec?.defaultRef) }}</dd></div>
            <div><dt>{{ t("project.createdAt") }}</dt><dd>{{ formatDate(project.metadata?.creationTimestamp) }}</dd></div>
          </dl>
        </article>

        <article class="content-panel">
          <div class="section-heading"><div><h2>{{ t("project.recentBuilds") }}</h2></div><button v-if="builds.length" class="text-button" type="button" @click="selectTab('builds')">{{ t("project.viewAllBuilds") }}</button></div>
          <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('project.loadingResources')"><span v-for="item in 4" :key="item"></span></div>
          <div v-else-if="resourcesError" class="inline-error compact-error"><WarningFilled /><span>{{ resourcesError }}</span></div>
          <EmptyState v-else-if="!builds.length" :title="t('project.emptyBuilds')" :description="t('project.emptyBuildsHint')" />
          <div v-else class="activity-list">
            <div v-for="build in builds.slice(0, 6)" :key="build.metadata?.name" class="activity-row">
              <div><strong>{{ build.metadata?.name }}</strong><small>{{ build.spec?.buildType || t("project.unspecifiedType") }} · {{ formatDate(build.status?.startTime) }}</small></div>
              <StatusBadge :value="build.status?.phase" />
            </div>
          </div>
        </article>
      </section>

      <div :class="['project-packages-layout', { 'has-selection': selectedPackageName }]">
      <article class="content-panel project-packages-panel">
        <div class="section-heading"><div><h2>{{ t("project.packageRepositories") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openPackageEditor"><Plus />{{ t("project.addPackage") }}</button></div></div>
        <div class="package-list-toolbar"><label class="search-box"><Search /><input v-model="packageSearch" type="search" :placeholder="t('project.searchPackages')" :aria-label="t('project.searchPackages')" /></label></div>
        <div v-if="packageSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.packageSaved") }}</div>
        <div v-if="deletedPackageName" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.packageDeleted", { name: deletedPackageName }) }}</div>
        <div v-if="filteredPackageRepos.length" class="project-table-wrap"><table class="project-table config-table package-table"><thead><tr><th>{{ t("project.repositoryName") }}</th><th v-if="!selectedPackageName">URL</th><th v-if="!selectedPackageName">Git ref</th><th v-if="canEditProject && !selectedPackageName" class="package-actions-column">{{ t("project.packageActions") }}</th></tr></thead><tbody><tr v-for="(repo, index) in paginatedPackageRepos" :key="`${repo.name}-${index}`" :class="{ 'selected-package-row': selectedPackageName === repo.name }"><td><button class="package-name-button" type="button" :aria-pressed="selectedPackageName === repo.name" :disabled="!repo.name" @click="selectPackage(repo.name || '')">{{ repo.name || t("common.emptyValue") }}</button></td><td v-if="!selectedPackageName"><code>{{ repo.url || t("common.emptyValue") }}</code></td><td v-if="!selectedPackageName">{{ gitRefLabel(repo.ref) }}</td><td v-if="canEditProject && !selectedPackageName" class="package-actions-column"><button class="text-button" type="button" :disabled="!repo.name" :aria-label="t('project.editPackage', { name: repo.name })" @click="openPackageEdit(repo)">{{ t("common.edit") }}</button><button class="text-button danger-link" type="button" :aria-label="t('project.deletePackage', { name: repo.name })" :disabled="!repo.name" @click="openPackageDelete(repo.name || '')">{{ t("project.deletePackageAction") }}</button></td></tr></tbody></table></div>
        <p v-else class="config-empty">{{ t(project.spec?.packageRepos?.length ? "project.noMatchingPackages" : "project.noPackageRepositories") }}</p>
        <div v-if="filteredPackageRepos.length" class="table-footer">
          <div class="page-summary">
            <span>{{ t("common.count", { count: filteredPackageRepos.length }) }}</span>
            <AppSelect :model-value="String(packagePageSize)" :options="packagePageSizes.map(size => ({ value: String(size), label: t('common.itemsPerPage', { count: size }) }))" :label="t('common.perPage')" compact @change="packagePageSize = Number($event); packageCurrentPage = 1" />
            <nav class="pagination-row" :aria-label="t('common.pagination')">
              <button class="page-button arrow-button" type="button" :aria-label="t('common.previous')" :disabled="packageCurrentPage === 1" @click="packageCurrentPage -= 1"><ArrowLeft /></button>
              <template v-for="item in packagePaginationItems" :key="item.key">
                <span v-if="item.page === null" class="page-ellipsis">…</span>
                <button v-else class="page-button" :class="{ active: item.page === packageCurrentPage }" type="button" :aria-current="item.page === packageCurrentPage ? 'page' : undefined" @click="packageCurrentPage = item.page">{{ item.page }}</button>
              </template>
              <button class="page-button arrow-button" type="button" :aria-label="t('common.next')" :disabled="packageCurrentPage === packageTotalPages" @click="packageCurrentPage += 1"><ArrowRight /></button>
            </nav>
          </div>
        </div>
      </article>
      <article v-if="selectedPackageName" class="content-panel package-detail-panel">
        <div class="section-heading"><div><h2>{{ selectedPackageName }}</h2></div><button class="package-detail-close" type="button" :aria-label="t('project.closePackageDetails')" @click="closePackageDetails"><Close /></button></div>
        <section class="package-detail-section">
          <h3>{{ t("project.rpmDownloadAddress") }}</h3>
          <p class="package-detail-placeholder">{{ t("project.rpmDownloadPending") }}</p>
        </section>
        <section class="package-detail-section">
          <div class="section-heading"><div><h3>{{ t("project.packageJobHistory") }}</h3></div><span v-if="!packageHistoryLoading && !packageHistoryErrorKey">{{ t("common.count", { count: packageHistoryJobs.length }) }}</span></div>
          <div v-if="packageHistoryLoading" class="skeleton-list" :aria-label="t('project.loadingPackageJobs')"><span v-for="item in 3" :key="item"></span></div>
          <div v-else-if="packageHistoryErrorKey" class="inline-error compact-error" role="alert"><WarningFilled /><span>{{ t(packageHistoryErrorKey) }}</span><button type="button" @click="loadPackageHistory">{{ t("common.reload") }}</button></div>
          <p v-else-if="!packageHistoryJobs.length" class="config-empty">{{ t("project.noPackageJobs") }}</p>
          <div v-else class="package-job-list">
            <div v-for="job in packageHistoryJobs" :key="job.metadata?.name" class="package-job-item">
              <div class="package-job-heading"><strong>{{ job.metadata?.name }}</strong><StatusBadge :value="job.status?.phase" /></div>
              <span>{{ job.metadata?.labels?.["ebs.io/target-os"] || t("common.emptyValue") }} · {{ job.metadata?.labels?.["ebs.io/target-arch"] || t("common.emptyValue") }}</span>
              <time>{{ formatDate(job.status?.startTime || job.metadata?.creationTimestamp) }}</time>
            </div>
          </div>
        </section>
      </article>
      </div>
    </section>

    <section v-else-if="activeTab === 'builds'" id="project-panel-builds" class="project-tab-panel" role="tabpanel" aria-labelledby="project-tab-builds">
      <div v-if="resourcesLoading" class="skeleton-list" :aria-label="t('project.loadingResources')"><span v-for="item in 6" :key="item"></span></div>
      <div v-else-if="buildLoadFailed" class="inline-error compact-error"><WarningFilled /><span>{{ t("errors.loadBuilds") }}</span></div>
      <EmptyState v-else-if="!builds.length" :title="t('project.emptyBuilds')" :description="t('project.emptyBuildsHint')" />
      <div v-else class="build-history-layout">
        <article class="content-panel build-history-list-panel">
          <div class="section-heading"><div><h2>{{ t("project.buildHistory") }}</h2></div><span>{{ t("common.count", { count: builds.length }) }}</span></div>
          <div class="build-history-list">
            <div v-for="build in builds" :key="build.metadata?.name" :class="['build-history-item', { active: selectedBuildName === build.metadata?.name, 'has-abort': canAbortBuild(build) }]">
              <button class="build-history-select" type="button" :aria-pressed="selectedBuildName === build.metadata?.name" @click="selectedBuildName = build.metadata?.name || ''">
                <span class="build-history-item-heading"><strong>{{ build.metadata?.name }}</strong><StatusBadge :value="build.status?.phase" /></span>
                <small>{{ build.spec?.buildType || t("project.unspecifiedType") }} · {{ buildTargetLabel(build) }}</small>
                <time>{{ formatDate(build.status?.startTime) }}</time>
              </button>
              <button v-if="canAbortBuild(build)" class="build-history-abort" type="button" :disabled="abortingBuild" @click="openAbortDialog(build)">{{ t("project.abortBuild") }}</button>
            </div>
          </div>
        </article>

        <article class="content-panel build-detail-panel">
          <template v-if="selectedBuild">
            <div class="section-heading"><div><h2>{{ selectedBuild.metadata?.name }}</h2><p>{{ t("project.buildDetailHint") }}</p></div><StatusBadge :value="selectedBuild.status?.phase" /></div>
            <dl class="detail-list build-detail-list">
              <div><dt>{{ t("project.buildType") }}</dt><dd>{{ selectedBuild.spec?.buildType || t("project.unspecifiedType") }}</dd></div>
              <div><dt>{{ t("project.buildTarget") }}</dt><dd>{{ buildTargetLabel(selectedBuild) }}</dd></div>
              <div><dt>{{ t("project.stage") }}</dt><dd>{{ selectedBuild.status?.stage || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.startedAt") }}</dt><dd>{{ formatDate(selectedBuild.status?.startTime) }}</dd></div>
              <div><dt>{{ t("project.finishedAt") }}</dt><dd>{{ formatDate(selectedBuild.status?.endTime) }}</dd></div>
              <div><dt>{{ t("project.baseBuild") }}</dt><dd>{{ baseBuildLabel(selectedBuild) }}</dd></div>
            </dl>
            <section class="build-detail-section"><h3>{{ t("project.packages") }}</h3><div v-if="selectedBuild.spec?.packages?.length" class="value-chip-list"><code v-for="item in selectedBuild.spec.packages" :key="item">{{ item }}</code></div><p v-else>{{ t("project.noPackages") }}</p></section>
          </template>
          <EmptyState v-else :title="t('project.selectBuild')" :description="t('project.selectBuildHint')" />
        </article>
      </div>
    </section>

    <section v-else id="project-panel-config" class="project-tab-panel config-layout" role="tabpanel" aria-labelledby="project-tab-config">
      <div class="config-split config-wide-panel">
        <div class="config-left-column">
          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.basicConfig") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openBasicEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="basicSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.basicConfigSaved") }}</div>
            <dl class="detail-list config-detail-list">
              <div><dt>{{ t("project.displayName") }}</dt><dd>{{ project.spec?.displayName || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.descriptionField") }}</dt><dd>{{ project.spec?.description || t("common.emptyValue") }}</dd></div>
              <div><dt>{{ t("project.defaultRef") }}</dt><dd><code>{{ gitRefLabel(project.spec?.defaultRef) }}</code></dd></div>
            </dl>
          </article>

          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.buildTargets") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openTargetEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="targetSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.targetsSaved") }}</div>
            <div v-if="project.spec?.buildTargets?.length" class="project-table-wrap">
              <table class="project-table target-table"><thead><tr><th>{{ t("projects.targetOS") }}</th><th>{{ t("projects.targetArch") }}</th><th>{{ t("project.buildEnabled") }}</th><th>{{ t("project.publishEnabled") }}</th></tr></thead><tbody><tr v-for="(target, index) in project.spec.buildTargets" :key="`${target.os}-${target.arch}-${index}`"><td><strong>{{ target.os || t("common.emptyValue") }}</strong></td><td>{{ target.arch || t("common.emptyValue") }}</td><td><span :class="['boolean-status', { enabled: target.buildFlag }]">{{ booleanLabel(target.buildFlag) }}</span></td><td><span :class="['boolean-status', { enabled: target.publishFlag }]">{{ booleanLabel(target.publishFlag) }}</span></td></tr></tbody></table>
            </div>
            <p v-else class="config-empty">{{ t("project.noBuildTargets") }}</p>
          </article>

          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.bootstrapRepositories") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openBootstrapEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="bootstrapSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.bootstrapSaved") }}</div>
            <div v-if="project.spec?.bootstrapRepo?.length" class="project-table-wrap"><table class="project-table config-table"><thead><tr><th>{{ t("project.repositoryName") }}</th><th>{{ t("project.repositoryAddress") }}</th></tr></thead><tbody><tr v-for="(repo, index) in project.spec.bootstrapRepo" :key="`${repo.name}-${index}`"><td><strong>{{ repo.name || t("common.emptyValue") }}</strong></td><td><code>{{ repo.repo || t("common.emptyValue") }}</code></td></tr></tbody></table></div>
            <p v-else class="config-empty">{{ t("project.noBootstrapRepositories") }}</p>
          </article>

          <article class="content-panel">
            <div class="section-heading"><div><h2>{{ t("project.userManagement") }}</h2></div><div class="section-actions"><button v-if="canEditProject" class="secondary-button compact-button" type="button" @click="openMemberEditor"><Edit />{{ t("common.edit") }}</button></div></div>
            <div v-if="memberSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.membersSaved") }}</div>
            <div class="project-user-list">
              <div class="project-user-row"><strong>{{ ownerUsername || t("common.emptyValue") }}</strong><span class="user-role owner-role">{{ t("project.ownerRole") }}</span></div>
              <div v-for="member in memberUsernames" :key="member" class="project-user-row"><strong>{{ member }}</strong><span class="user-role">{{ t("project.memberRole") }}</span></div>
            </div>
            <p v-if="!memberUsernames.length" class="config-empty user-empty">{{ t("project.noMembers") }}</p>
          </article>
        </div>

        <article class="content-panel payload-panel">
          <div class="section-heading"><div><h2>{{ t("project.buildPayload") }}</h2></div><div class="section-actions"><button v-if="canEditProject && !payloadEditing" class="secondary-button compact-button" type="button" @click="startPayloadEditing"><Edit />{{ t("common.edit") }}</button><template v-else-if="payloadEditing"><button class="secondary-button compact-button" type="button" :disabled="savingPayload" @click="cancelPayloadEditing">{{ t("common.cancel") }}</button><button class="primary-button compact-button" type="button" :disabled="savingPayload" @click="savePayload">{{ savingPayload ? t("common.saving") : t("common.save") }}</button></template></div></div>
          <div v-if="payloadSaveSuccess" class="success-banner compact-banner" role="status"><CircleCheckFilled />{{ t("project.payloadSaved") }}</div>
          <template v-if="payloadEditing">
            <textarea v-model="payloadDraft" class="payload-content payload-editor" spellcheck="false" :aria-label="t('project.buildPayload')"></textarea>
            <div v-if="payloadErrorKey" class="form-error payload-error" role="alert"><WarningFilled />{{ t(payloadErrorKey) }}</div>
          </template>
          <template v-else><pre v-if="project.spec?.buildPayload" class="payload-content">{{ project.spec.buildPayload }}</pre><p v-else class="config-empty">{{ t("project.noBuildPayload") }}</p></template>
        </article>
      </div>

    </section>

    <ModalDialog v-if="buildDialogOpen" title-id="create-build-title" :title="t(buildDialogTitle)" :close-label="t('common.close')" @close="closeBuildDialog">
      <form class="project-form" @submit.prevent="createBuild">
        <p class="form-hint">{{ t("project.createBuildHint") }}</p>
        <fieldset v-if="requiresBuildPackages" class="target-fieldset build-package-fieldset">
          <legend>{{ t("project.selectBuildPackages") }}</legend>
          <p class="form-hint build-package-hint">{{ t("project.selectBuildPackagesHint") }}</p>
          <label class="search-box build-package-search"><Search /><input v-model="buildPackageSearch" type="search" :placeholder="t('project.searchBuildPackages')" :aria-label="t('project.searchBuildPackages')" /></label>
          <div class="build-package-options">
            <label v-for="packageName in visibleBuildPackages" :key="packageName" class="build-package-option"><input v-model="selectedBuildPackages" type="checkbox" :value="packageName" :disabled="creatingBuild" /><span>{{ packageName }}</span></label>
            <p v-if="!visibleBuildPackages.length" class="form-hint">{{ t("project.noMatchingBuildPackages") }}</p>
          </div>
          <p class="build-package-count">{{ t("project.selectedBuildPackages", { count: selectedBuildPackages.length }) }}</p>
        </fieldset>
        <p v-if="unsupportedBuildSelection" class="form-error">{{ t('buildConf.unsupported') }} <button type="button" @click="reloadBuildConf">{{ t('common.reload') }}</button></p>
        <fieldset class="target-fieldset">
          <legend>{{ t("project.buildTarget") }}</legend>
          <div class="build-target-options">
            <div v-for="(target, index) in buildTargetDrafts" :key="`${target.os}-${target.arch}-${index}`" class="build-target-option">
              <label class="build-target-selection"><input v-model="target.selected" type="checkbox" :disabled="creatingBuild" /><span>{{ targetListLabel([target]) }} <small v-if="!supportsBuildTarget(target)">{{ t('buildConf.unsupported') }}</small></span></label>
              <div class="build-target-statuses">
                <span>{{ t("project.buildEnabled") }} <span :class="['target-boolean-icon', { enabled: target.buildFlag }]" :aria-label="booleanLabel(target.buildFlag)" :title="booleanLabel(target.buildFlag)"><Check v-if="target.buildFlag" /><Close v-else /></span></span>
                <span>{{ t("project.publishEnabled") }} <span :class="['target-boolean-icon', { enabled: target.publishFlag }]" :aria-label="booleanLabel(target.publishFlag)" :title="booleanLabel(target.publishFlag)"><Check v-if="target.publishFlag" /><Close v-else /></span></span>
              </div>
            </div>
          </div>
        </fieldset>
        <div v-if="buildDialogErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(buildDialogErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="creatingBuild" @click="closeBuildDialog">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="creatingBuild || unsupportedBuildSelection || (requiresBuildPackages && !selectedBuildPackages.length)">{{ creatingBuild ? t("project.creatingBuild") : t("project.startBuild") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="abortTarget" title-id="abort-build-title" :title="t('project.abortBuild')" :close-label="t('common.close')" @close="closeAbortDialog">
      <form class="project-form" @submit.prevent="confirmAbort">
        <p class="form-hint">{{ t("project.abortBuildConfirm", { name: abortTarget.metadata?.name }) }}</p>
        <div class="abort-build-phase"><span>{{ t("project.currentBuildPhase") }}</span><StatusBadge :value="abortTarget.status?.phase" /></div>
        <p class="form-hint">{{ t("project.abortBuildWarning") }}</p>
        <div v-if="abortErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(abortErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="abortingBuild" @click="closeAbortDialog">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="submit" :disabled="abortingBuild">{{ abortingBuild ? t("project.abortingBuild") : t("project.abortBuild") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="basicEditorOpen" title-id="edit-basic-title" :title="t('project.editBasicConfig')" :close-label="t('common.close')" @close="closeBasicEditor">
      <form class="project-form" @submit.prevent="saveBasicConfig">
        <p class="form-hint">{{ t("project.editBasicConfigHint") }}</p>
        <label class="field"><span>{{ t("project.displayName") }}</span><input v-model.trim="basicDraft.displayName" :disabled="savingBasic" autocomplete="off" /></label>
        <label class="field"><span>{{ t("project.descriptionField") }}</span><textarea v-model.trim="basicDraft.description" :disabled="savingBasic" rows="3"></textarea></label>
        <div class="form-grid package-ref-fields">
          <div class="field required-field"><span>{{ t("project.refType") }}</span><AppSelect :model-value="basicDraft.defaultRef.type" :options="[{ value: 'Branch', label: t('project.refBranch') }, { value: 'Tag', label: t('project.refTag') }]" :label="t('project.refType')" :disabled="savingBasic" @update:model-value="basicDraft.defaultRef.type = $event as 'Branch' | 'Tag'" /></div>
          <label class="field required-field"><span>{{ t("project.defaultRef") }}</span><input v-model.trim="basicDraft.defaultRef.value" required :disabled="savingBasic" autocomplete="off" /></label>
        </div>
        <div v-if="basicErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(basicErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingBasic" @click="closeBasicEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingBasic">{{ savingBasic ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="packageEditorOpen" title-id="add-package-title" :title="t(editingPackageName ? 'project.editPackage' : 'project.addPackage', { name: editingPackageName })" :close-label="t('common.close')" @close="closePackageEditor">
      <form class="project-form" @submit.prevent="savePackage">
        <p class="form-hint">{{ t(editingPackageName ? "project.editPackageHint" : "project.addPackageHint") }}</p>
        <label class="field required-field"><span>{{ t("project.repositoryName") }}</span><input v-model.trim="packageDraft.name" required :disabled="savingPackage || Boolean(editingPackageName)" autocomplete="off" placeholder="gcc" /></label>
        <label class="field required-field"><span>{{ t("project.repositoryAddress") }}</span><input v-model.trim="packageDraft.url" required :disabled="savingPackage" autocomplete="off" placeholder="https://atomgit.com/src-openeuler/gcc.git" /></label>
        <div class="form-grid package-ref-fields">
          <div class="field"><span>{{ t("project.refType") }}</span><AppSelect :model-value="packageDraft.ref.type" :options="[{ value: 'Branch', label: t('project.refBranch') }, { value: 'Tag', label: t('project.refTag') }, { value: 'Commit', label: 'Commit' }]" :label="t('project.refType')" :disabled="savingPackage" @update:model-value="packageDraft.ref.type = $event as NonNullable<GitRef['type']>" /></div>
          <label class="field"><span>{{ t("project.refValue") }}</span><input v-model.trim="packageDraft.ref.value" :disabled="savingPackage" autocomplete="off" /></label>
        </div>
        <div v-if="packageErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(packageErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingPackage" @click="closePackageEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingPackage">{{ savingPackage ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="packageDeleteName" title-id="delete-package-title" :title="t('project.deletePackage', { name: packageDeleteName })" :close-label="t('common.close')" @close="closePackageDelete">
      <p class="form-hint">{{ t("project.deletePackageConfirm", { name: packageDeleteName }) }}</p>
      <div v-if="packageDeleteErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(packageDeleteErrorKey) }}</div>
      <div class="modal-actions"><button class="secondary-button" type="button" :disabled="deletingPackage" @click="closePackageDelete">{{ t("common.cancel") }}</button><button class="primary-button danger-button" type="button" :disabled="deletingPackage" @click="confirmDeletePackage">{{ deletingPackage ? t("common.saving") : t("project.deletePackageAction") }}</button></div>
    </ModalDialog>

    <ModalDialog v-if="targetEditorOpen" title-id="edit-targets-title" :title="t('project.editBuildTargets')" :close-label="t('common.close')" @close="closeTargetEditor">
      <form class="project-form" @submit.prevent="saveTargets">
        <p class="form-hint">{{ t("project.editTargetsHint") }}</p>
        <div class="editable-target-list">
          <fieldset v-for="(target, index) in editingTargets" :key="index" class="target-fieldset editable-target">
            <legend>{{ t("project.targetNumber", { number: index + 1 }) }}</legend>
            <button class="remove-target-button" type="button" :aria-label="t('project.removeTarget', { number: index + 1 })" :disabled="editingTargets.length === 1 || savingTargets" @click="removeTarget(index)"><Delete /></button>
            <BuildTargetFields v-model:os="target.os" v-model:arch="target.arch" allow-legacy />
            <div class="checkbox-row"><label><input v-model="target.buildFlag" type="checkbox" />{{ t("projects.buildFlag") }}</label><label><input v-model="target.publishFlag" type="checkbox" />{{ t("projects.publishFlag") }}</label></div>
          </fieldset>
        </div>
        <button class="secondary-button add-target-button" type="button" :disabled="savingTargets" @click="addTarget"><Plus />{{ t("project.addTarget") }}</button>
        <div v-if="targetErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(targetErrorKey, targetErrorParams) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingTargets" @click="closeTargetEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingTargets">{{ savingTargets ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="bootstrapEditorOpen" title-id="edit-bootstrap-title" :title="t('project.editBootstrapRepositories')" :close-label="t('common.close')" @close="closeBootstrapEditor">
      <form class="project-form" @submit.prevent="saveBootstrapRepositories">
        <p class="form-hint">{{ t("project.editBootstrapHint") }}</p>
        <div v-if="editingBootstrapRepositories.length" class="editable-target-list">
          <fieldset v-for="(repo, index) in editingBootstrapRepositories" :key="index" class="target-fieldset editable-target">
            <legend>{{ t("project.bootstrapNumber", { number: index + 1 }) }}</legend>
            <button class="remove-target-button" type="button" :aria-label="t('project.removeBootstrap', { number: index + 1 })" :disabled="savingBootstrap" @click="removeBootstrapRepository(index)"><Delete /></button>
            <div class="form-grid bootstrap-fields">
              <label class="field required-field repository-name-field"><span>{{ t("project.sourceName") }}</span><input v-model.trim="repo.name" required autocomplete="off" :placeholder="t('project.repositoryNamePlaceholder')" /></label>
              <label class="field required-field"><span>{{ t("project.sourceAddress") }}</span><input v-model.trim="repo.repo" required autocomplete="off" :placeholder="t('project.repositoryAddressPlaceholder')" /></label>
            </div>
          </fieldset>
        </div>
        <p v-else class="config-empty editor-empty">{{ t("project.noBootstrapDraft") }}</p>
        <button class="secondary-button add-target-button" type="button" :disabled="savingBootstrap" @click="addBootstrapRepository"><Plus />{{ t("project.addBootstrap") }}</button>
        <div v-if="bootstrapErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(bootstrapErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingBootstrap" @click="closeBootstrapEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingBootstrap">{{ savingBootstrap ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="memberEditorOpen" title-id="edit-members-title" :title="t('project.editMembers')" :close-label="t('common.close')" @close="closeMemberEditor">
      <form class="project-form" @submit.prevent="saveMembers">
        <p class="form-hint">{{ t("project.editMembersHint") }}</p>
        <div class="member-owner-row"><span>{{ t("project.ownerRole") }}</span><strong>{{ ownerUsername }}</strong></div>
        <div v-if="editingMembers.length" class="editable-member-list">
          <div v-for="(member, index) in editingMembers" :key="member" class="editable-member-row">
            <strong>{{ member }}</strong>
            <button class="remove-member-button" type="button" :disabled="savingMembers" :aria-label="t('project.removeMember', { username: member })" @click="removeMember(index)"><Delete />{{ t("common.remove") }}</button>
          </div>
        </div>
        <p v-else class="config-empty editor-empty">{{ t("project.noMembers") }}</p>
        <div class="member-add-row">
          <label class="field"><span>{{ t("project.memberUsername") }}</span><input v-model.trim="memberDraft" autocomplete="off" :placeholder="t('project.memberUsernamePlaceholder')" @keydown.enter.prevent="addMember" /></label>
          <button class="secondary-button" type="button" :disabled="savingMembers" @click="addMember"><Plus />{{ t("project.addMember") }}</button>
        </div>
        <div v-if="memberErrorKey" class="form-error" role="alert"><WarningFilled />{{ t(memberErrorKey) }}</div>
        <div class="modal-actions"><button class="secondary-button" type="button" :disabled="savingMembers" @click="closeMemberEditor">{{ t("common.cancel") }}</button><button class="primary-button" type="submit" :disabled="savingMembers">{{ savingMembers ? t("common.saving") : t("common.save") }}</button></div>
      </form>
    </ModalDialog>
  </template>
</template>

<script setup lang="ts">
import { ArrowLeft, ArrowRight, Check, CircleCheckFilled, Close, Delete, DocumentCopy, Download, Edit, Operation, Plus, Search, Tickets, WarningFilled } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import { stringify } from "yaml";

import { ApiError, errorTranslationKey, list, request } from "@/api";
import EmptyState from "@/components/EmptyState.vue";
import AppSelect from "@/components/AppSelect.vue";
import ModalDialog from "@/components/ModalDialog.vue";
import BuildTargetFields from "@/components/BuildTargetFields.vue";
import { useBuildConf } from "@/composables/useBuildConf";
import StatusBadge from "@/components/StatusBadge.vue";
import { useSessionStore } from "@/stores/session";
import ProjectJobs from "@/components/ProjectJobs.vue";
import { PACKAGE_NAME_LABEL, packageNameLabelValue } from "@/utils/packageLabel";
import type { BootstrapRepo, Build, BuildTarget, GitRef, Job, PackageRepo, Project } from "@/types";

type ProjectTab = "overview" | "builds" | "config";
type BuildType = "full" | "incremental" | "single" | "specified";
type BuildTargetDraft = BuildTarget & { selected: boolean };

const route = useRoute();
const router = useRouter();
const session = useSessionStore();
const { t } = useI18n();
const name = computed(() => String(route.params.name || ""));
const project = ref<Project | null>(null);
const builds = ref<Build[]>([]);
const selectedBuildName = ref("");
const jobs = ref<Job[]>([]);
const loadingProject = ref(true);
const resourcesLoading = ref(true);
const projectErrorKey = ref("");
const resourceFailureCount = ref(0);
const buildLoadFailed = ref(false);
const exportErrorKey = ref("");
const copyErrorKey = ref("");
const copiedId = ref(false);
let copyResetTimer: number | undefined;
const buildDialogOpen = ref(false);
const buildType = ref<BuildType>("full");
const selectedBuildPackages = ref<string[]>([]);
const buildPackageSearch = ref("");
const requiresBuildPackages = computed(() => buildType.value === "single" || buildType.value === "specified");
const buildDialogTitle = computed(() => ({ full: "project.createFullBuild", incremental: "project.createIncrementalBuild", single: "project.createSingleBuild", specified: "project.createSpecifiedBuild" })[buildType.value]);
const availableBuildPackages = computed(() => [...new Set((project.value?.spec?.packageRepos || []).map((repo) => repo.name).filter((value): value is string => Boolean(value?.trim())))].sort((left, right) => left.localeCompare(right)));
const visibleBuildPackages = computed(() => availableBuildPackages.value.filter((item) => item.toLowerCase().includes(buildPackageSearch.value.trim().toLowerCase())));
const buildTargetDrafts = ref<BuildTargetDraft[]>([]);
const creatingBuild = ref(false);
const { supports: supportsBuildTarget, error: buildConfError, loading: buildConfLoading, reload: reloadBuildConf } = useBuildConf();
const unsupportedBuildSelection = computed(() => buildConfLoading.value || Boolean(buildConfError.value) || buildTargetDrafts.value.some(target => target.selected && !supportsBuildTarget(target)));
const buildDialogErrorKey = ref("");
const buildActionErrorKey = ref("");
const createdBuildNames = ref<string[]>([]);
const abortTarget = ref<Build | null>(null);
const abortingBuild = ref(false);
const abortErrorKey = ref("");
const abortedBuildName = ref("");
const targetEditorOpen = ref(false);
const editingTargets = ref<BuildTarget[]>([]);
const savingTargets = ref(false);
const targetErrorKey = ref("");
const targetErrorParams = ref<Record<string, string>>({});
const targetSaveSuccess = ref(false);
const bootstrapEditorOpen = ref(false);
const editingBootstrapRepositories = ref<BootstrapRepo[]>([]);
const savingBootstrap = ref(false);
const bootstrapErrorKey = ref("");
const bootstrapSaveSuccess = ref(false);
const payloadEditing = ref(false);
const payloadDraft = ref("");
const savingPayload = ref(false);
const payloadErrorKey = ref("");
const payloadSaveSuccess = ref(false);
const basicEditorOpen = ref(false);
const savingBasic = ref(false);
const basicErrorKey = ref("");
const basicSaveSuccess = ref(false);
const basicDraft = ref({ displayName: "", description: "", defaultRef: { type: "Branch" as "Branch" | "Tag", value: "" } });
const memberEditorOpen = ref(false);
const editingMembers = ref<string[]>([]);
const memberDraft = ref("");
const savingMembers = ref(false);
const memberErrorKey = ref("");
const memberSaveSuccess = ref(false);
const packageSearch = ref("");
const selectedPackageName = ref("");
const packageHistoryJobs = ref<Job[]>([]);
const packageHistoryLoading = ref(false);
const packageHistoryErrorKey = ref("");
let packageHistoryRequestId = 0;
const packageEditorOpen = ref(false);
const editingPackageName = ref("");
const packageEditorProject = ref<Project | null>(null);
const savingPackage = ref(false);
const packageErrorKey = ref("");
const packageSaveSuccess = ref(false);
const packageDeleteName = ref("");
const packageDeleteErrorKey = ref("");
const deletingPackage = ref(false);
const deletedPackageName = ref("");
const packagePageSizes = [20, 50, 100] as const;
const packagePageSize = ref<number>(20);
const packageCurrentPage = ref(1);
const packageDraft = ref({ name: "", url: "", ref: { type: "Branch" as NonNullable<GitRef["type"]>, value: "" } });
const filteredPackageRepos = computed(() => {
  const repos = project.value?.spec?.packageRepos || [];
  const query = packageSearch.value.trim().toLowerCase();
  return query ? repos.filter((repo) => [repo.name, repo.url, repo.ref?.type, repo.ref?.value]
    .some((value) => value?.toLowerCase().includes(query))) : repos;
});
const packageTotalPages = computed(() => Math.max(1, Math.ceil(filteredPackageRepos.value.length / packagePageSize.value)));
const paginatedPackageRepos = computed(() => {
  const start = (packageCurrentPage.value - 1) * packagePageSize.value;
  return filteredPackageRepos.value.slice(start, start + packagePageSize.value);
});
const packagePaginationItems = computed(() => buildPaginationItems(packageTotalPages.value, packageCurrentPage.value));
const activeTab = ref<ProjectTab>(normalizeTab(route.query.tab));
const projectError = computed(() => (projectErrorKey.value ? t(projectErrorKey.value) : ""));
const resourcesError = computed(() =>
  resourceFailureCount.value ? t("project.partialFailure", { count: resourceFailureCount.value }) : "",
);
const tabs = computed<Array<{ id: ProjectTab; label: string }>>(() => [
  { id: "overview", label: t("project.overviewTab") },
  { id: "builds", label: t("project.buildHistoryTab") },
  { id: "config", label: t("project.configTab") },
]);
const canEditProject = computed(() => {
  const identity = session.session?.identity;
  if (!identity) return false;
  if (identity.type === "admin" || identity.scopes.includes("ebs:system")) return true;
  return project.value?.metadata?.labels?.["ebs.io/owner-user"] === identity.name;
});
const canStartBuild = computed(() => {
  const identity = session.session?.identity;
  if (!identity) return false;
  if (identity.type === "admin" || identity.scopes.includes("ebs:system")) return true;
  const labels = project.value?.metadata?.labels || {};
  return labels["ebs.io/owner-user"] === identity.name || labels[`ebs.io/member-user.${identity.name}`] === "true";
});
const canAbortJobs = computed(() => {
  const identity = session.session?.identity;
  if (!identity || identity.type === 'service') return false;
  const labels = project.value?.metadata?.labels || {};
  return labels['ebs.io/owner-user'] === identity.name || labels[`ebs.io/member-user.${identity.name}`] === 'true';
});
const buildConfigurationMissing = computed(() =>
  !project.value?.spec?.buildTargets?.length || !project.value?.spec?.packageRepos?.some((repo) => repo.name),
);
const ownerUsername = computed(() => project.value?.metadata?.labels?.["ebs.io/owner-user"] || "");
const memberUsernames = computed(() =>
  Object.entries(project.value?.metadata?.labels || {})
    .filter(([key, value]) => key.startsWith("ebs.io/member-user.") && value === "true")
    .map(([key]) => key.slice("ebs.io/member-user.".length))
    .filter(Boolean)
    .sort((left, right) => left.localeCompare(right)),
);
const selectedBuild = computed(() => builds.value.find((build) => build.metadata?.name === selectedBuildName.value) || null);

function canAbortBuild(build: Build): boolean {
  return canStartBuild.value && Boolean(build.metadata?.name) && ["Pending", "Prepared", "Processing"].includes(build.status?.phase || "");
}

function openAbortDialog(build: Build): void {
  if (!canAbortBuild(build)) return;
  abortTarget.value = build;
  abortErrorKey.value = "";
  buildActionErrorKey.value = "";
  createdBuildNames.value = [];
  abortedBuildName.value = "";
}

function closeAbortDialog(): void {
  if (!abortingBuild.value) abortTarget.value = null;
}

async function confirmAbort(): Promise<void> {
  const build = abortTarget.value;
  const buildName = build?.metadata?.name;
  if (!build || !buildName || !canAbortBuild(build) || abortingBuild.value) return;
  abortingBuild.value = true;
  abortErrorKey.value = "";
  try {
    await request<Build>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}/builds/${encodeURIComponent(buildName)}/abort`, { method: "POST" });
    abortTarget.value = null;
    abortedBuildName.value = buildName;
    await loadResources();
  } catch (error) {
    if (error instanceof ApiError && (error.status === 404 || error.status === 409)) {
      abortTarget.value = null;
      buildActionErrorKey.value = "project.abortBuildStale";
      await loadResources();
    } else {
      abortErrorKey.value = errorTranslationKey(error, "project.abortBuildFailed");
    }
  } finally {
    abortingBuild.value = false;
  }
}

onMounted(async () => {
  await loadProject();
  if (project.value) await loadResources();
});

onBeforeUnmount(() => window.clearTimeout(copyResetTimer));

watch(packageSearch, () => {
  packageCurrentPage.value = 1;
});

watch(packageTotalPages, (total) => {
  if (packageCurrentPage.value > total) packageCurrentPage.value = total;
});

async function loadProject(): Promise<void> {
  loadingProject.value = true;
  projectErrorKey.value = "";
  try {
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`);
  } catch (reason) {
    projectErrorKey.value = errorTranslationKey(reason, "errors.loadProject");
  } finally {
    loadingProject.value = false;
  }
}

async function loadResources(): Promise<void> {
  resourcesLoading.value = true;
  const base = `/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`;
  const results = await Promise.allSettled([
    list<Build>(`${base}/builds?limit=100`),
    list<Job>(`${base}/jobs?limit=20`),
  ]);
  if (results[0].status === "fulfilled") {
    builds.value = results[0].value.items;
    if (!builds.value.some((build) => build.metadata?.name === selectedBuildName.value)) {
      selectedBuildName.value = builds.value[0]?.metadata?.name || "";
    }
  }
  if (results[1].status === "fulfilled") jobs.value = results[1].value.items;
  const failures = results.filter((item) => item.status === "rejected");
  buildLoadFailed.value = results[0].status === "rejected";
  resourceFailureCount.value = failures.length;
  resourcesLoading.value = false;
}

function selectTab(tab: ProjectTab): void {
  activeTab.value = tab;
  const query = { ...route.query };
  if (tab === "overview") delete query.tab;
  else query.tab = tab;
  void router.replace({ query });
}

function openBuildDialog(type: BuildType): void {
  if (!project.value || !canStartBuild.value || buildConfigurationMissing.value) return;
  void reloadBuildConf();
  buildType.value = type;
  selectedBuildPackages.value = [];
  buildPackageSearch.value = "";
  buildTargetDrafts.value = (project.value.spec?.buildTargets || []).map((target) => ({
    os: target.os,
    arch: target.arch,
    buildFlag: Boolean(target.buildFlag),
    publishFlag: Boolean(target.publishFlag),
    selected: true,
  }));
  buildDialogErrorKey.value = "";
  buildActionErrorKey.value = "";
  createdBuildNames.value = [];
  abortedBuildName.value = "";
  buildDialogOpen.value = true;
}

function closeBuildDialog(): void {
  if (creatingBuild.value) return;
  buildDialogOpen.value = false;
  buildDialogErrorKey.value = "";
}

async function createBuild(): Promise<void> {
  if (unsupportedBuildSelection.value) return;
  if (!project.value || !canStartBuild.value || creatingBuild.value) return;
  const targets = buildTargetDrafts.value
    .filter((target) => target.selected && target.os && target.arch)
    .map(({ selected: _, ...target }) => target);
  const hasPackages = (project.value.spec?.packageRepos || []).some((repo) => Boolean(repo.name?.trim()));
  if (!targets.length) {
    buildDialogErrorKey.value = "project.selectBuildTarget";
    return;
  }
  if (!hasPackages) {
    buildDialogErrorKey.value = "project.buildConfigurationRequired";
    return;
  }
  if (requiresBuildPackages.value && !selectedBuildPackages.value.length) {
    buildDialogErrorKey.value = "project.selectBuildPackagesError";
    return;
  }
  creatingBuild.value = true;
  buildDialogErrorKey.value = "";
  const selectedType = buildType.value;
  const packages = requiresBuildPackages.value ? [...selectedBuildPackages.value] : undefined;
  const buildNames = targets.map(() => createBuildName());
  const results = await Promise.allSettled(targets.map((target, index) =>
    request<Build>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}/builds`, {
      method: "POST",
      body: JSON.stringify({ apiVersion: "ebs/v1", kind: "Build", metadata: { name: buildNames[index] }, spec: { buildType: selectedType, buildTarget: { ...target }, ...(packages ? { packages } : {}) } }),
    }),
  ));
  const successfulNames = results.flatMap((result, index) => result.status === "fulfilled" ? [result.value.metadata?.name || buildNames[index]] : []);
  const failures = results.flatMap((result) => result.status === "rejected" ? [result.reason] : []);
  try {
    if (!successfulNames.length) {
      const reason = failures.find((failure) => !(failure instanceof ApiError && failure.status === 409)) ?? failures[0];
      buildDialogErrorKey.value = reason instanceof ApiError && reason.status === 409 ? "project.activeBuildConflict" : errorTranslationKey(reason, "errors.createBuild");
      return;
    }
    buildDialogOpen.value = false;
    createdBuildNames.value = successfulNames;
    selectedBuildName.value = successfulNames[0];
    if (successfulNames.length < targets.length) {
      const conflictsOnly = failures.every((failure) => failure instanceof ApiError && failure.status === 409);
      const missingBaselineOnly = failures.every((failure) => failure instanceof ApiError && failure.translationKey === "project.fullBuildRequired");
      buildActionErrorKey.value = conflictsOnly ? "project.partialActiveBuildConflict" : missingBaselineOnly ? "project.partialFullBuildRequired" : "project.partialBuildFailure";
    }
    await loadResources();
    selectTab("builds");
  } finally {
    creatingBuild.value = false;
  }
}

function createBuildName(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function openTargetEditor(): void {
  if (!project.value || !canEditProject.value) return;
  targetSaveSuccess.value = false;
  targetErrorKey.value = "";
  editingTargets.value = (project.value.spec?.buildTargets || []).map((target) => ({
    os: target.os || "",
    arch: target.arch || "",
    buildFlag: Boolean(target.buildFlag),
    publishFlag: Boolean(target.publishFlag),
  }));
  if (!editingTargets.value.length) addTarget();
  targetEditorOpen.value = true;
}

function openPackageEditor(): void {
  if (!project.value || !canEditProject.value) return;
  editingPackageName.value = "";
  packageEditorProject.value = JSON.parse(JSON.stringify(project.value)) as Project;
  packageDraft.value = { name: "", url: "", ref: { type: project.value.spec?.defaultRef?.type || "Branch", value: project.value.spec?.defaultRef?.value || "master" } };
  packageErrorKey.value = "";
  packageSaveSuccess.value = false;
  deletedPackageName.value = "";
  packageEditorOpen.value = true;
}

function openPackageEdit(repo: PackageRepo): void {
  if (!project.value || !canEditProject.value || !repo.name || savingPackage.value) return;
  packageEditorProject.value = JSON.parse(JSON.stringify(project.value)) as Project;
  editingPackageName.value = repo.name;
  packageDraft.value = { name: repo.name, url: repo.url || "", ref: { type: repo.ref?.type || "Branch", value: repo.ref?.value || "" } };
  packageErrorKey.value = "";
  packageSaveSuccess.value = false;
  deletedPackageName.value = "";
  packageEditorOpen.value = true;
}

function closePackageEditor(): void {
  if (savingPackage.value) return;
  packageEditorOpen.value = false;
  packageErrorKey.value = "";
}

async function savePackage(): Promise<void> {
  if (!project.value || !canEditProject.value || savingPackage.value) return;
  const draft = packageDraft.value;
  const repo: PackageRepo = { name: editingPackageName.value || draft.name.trim(), url: draft.url.trim(), ref: draft.ref.value.trim() ? { type: draft.ref.type, value: draft.ref.value.trim() } : undefined };
  if (!repo.name || !repo.url) {
    packageErrorKey.value = "project.packageRequired";
    return;
  }
  if (!editingPackageName.value && packageEditorProject.value?.spec?.packageRepos?.some((item) => item.name === repo.name)) {
    packageErrorKey.value = "project.packageAlreadyAdded";
    return;
  }
  if (repo.ref?.type === "Commit" && !/^[a-fA-F0-9]{40}$/.test(repo.ref.value || "")) {
    packageErrorKey.value = "project.invalidCommit";
    return;
  }
  if (repo.ref?.type === "Commit") repo.ref.value = repo.ref.value?.toLowerCase();
  savingPackage.value = true;
  packageErrorKey.value = "";
  try {
    if (!packageEditorProject.value) throw new Error("missing project input");
    const updated = JSON.parse(JSON.stringify(packageEditorProject.value)) as Project;
    const repos = updated.spec?.packageRepos || [];
    if (editingPackageName.value && !repos.some((item) => item.name === editingPackageName.value)) {
      packageErrorKey.value = "errors.conflict";
      return;
    }
    updated.spec = { ...updated.spec, packageRepos: editingPackageName.value
      ? repos.map((item) => item.name === editingPackageName.value ? { ...item, url: repo.url, ref: repo.ref } : item)
      : [...repos, repo] };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    packageEditorOpen.value = false;
    packageSaveSuccess.value = true;
    packageSearch.value = "";
    packageCurrentPage.value = 1;
  } catch (reason) {
    packageErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingPackage.value = false;
  }
}

function openPackageDelete(packageName: string): void {
  if (!project.value || !canEditProject.value || !packageName) return;
  packageDeleteName.value = packageName;
  packageDeleteErrorKey.value = "";
  packageSaveSuccess.value = false;
  deletedPackageName.value = "";
}

function selectPackage(packageName: string): void {
  if (!packageName) return;
  selectedPackageName.value = packageName;
  void loadPackageHistory();
}

function closePackageDetails(): void {
  packageHistoryRequestId += 1;
  selectedPackageName.value = "";
  packageHistoryJobs.value = [];
  packageHistoryErrorKey.value = "";
  packageHistoryLoading.value = false;
}

async function listPackageJobs(packageLabelValue: string): Promise<Job[]> {
  const items: Job[] = [];
  let next = "";
  do {
    const query = new URLSearchParams({ limit: "100", labelSelector: `${PACKAGE_NAME_LABEL}=${packageLabelValue}` });
    if (next) query.set("continue", next);
    const page = await list<Job>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}/jobs?${query}`);
    items.push(...page.items);
    next = page.next;
  } while (next);
  return items;
}

async function loadPackageHistory(): Promise<void> {
  const packageName = selectedPackageName.value;
  if (!packageName) return;
  const requestId = ++packageHistoryRequestId;
  packageHistoryLoading.value = true;
  packageHistoryErrorKey.value = "";
  packageHistoryJobs.value = [];
  try {
    const labelValue = await packageNameLabelValue(packageName);
    const jobs = await listPackageJobs(labelValue);
    if (requestId !== packageHistoryRequestId) return;
    packageHistoryJobs.value = jobs.sort((left, right) => (Date.parse(right.status?.startTime || right.metadata?.creationTimestamp || "") || 0) - (Date.parse(left.status?.startTime || left.metadata?.creationTimestamp || "") || 0));
  } catch (reason) {
    if (requestId === packageHistoryRequestId) packageHistoryErrorKey.value = errorTranslationKey(reason, "project.loadPackageJobsFailed");
  } finally {
    if (requestId === packageHistoryRequestId) packageHistoryLoading.value = false;
  }
}

function closePackageDelete(): void {
  if (deletingPackage.value) return;
  packageDeleteName.value = "";
  packageDeleteErrorKey.value = "";
}

async function confirmDeletePackage(): Promise<void> {
  if (!project.value || !canEditProject.value || deletingPackage.value || !packageDeleteName.value) return;
  const packageName = packageDeleteName.value;
  if (!project.value.spec?.packageRepos?.some((repo) => repo.name === packageName)) {
    packageDeleteErrorKey.value = "project.packageDeleteMissing";
    return;
  }
  deletingPackage.value = true;
  packageDeleteErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, packageRepos: (updated.spec?.packageRepos || []).filter((repo) => repo.name !== packageName) };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    if (selectedPackageName.value === packageName) closePackageDetails();
    packageDeleteName.value = "";
    deletedPackageName.value = packageName;
    packageCurrentPage.value = Math.min(packageCurrentPage.value, packageTotalPages.value);
  } catch (reason) {
    packageDeleteErrorKey.value = errorTranslationKey(reason, "project.packageDeleteFailed");
  } finally {
    deletingPackage.value = false;
  }
}

function closeTargetEditor(): void {
  if (savingTargets.value) return;
  targetEditorOpen.value = false;
  targetErrorKey.value = "";
}

function addTarget(): void {
  editingTargets.value.push({ os: "", arch: "", buildFlag: true, publishFlag: false });
}

function removeTarget(index: number): void {
  if (editingTargets.value.length > 1) editingTargets.value.splice(index, 1);
}

async function saveTargets(): Promise<void> {
  if (!project.value) return;
  targetErrorParams.value = {};
  const targets = editingTargets.value.map((target) => ({
    os: target.os?.trim(),
    arch: target.arch?.trim(),
    buildFlag: Boolean(target.buildFlag),
    publishFlag: Boolean(target.publishFlag),
  }));
  if (!targets.length || targets.some((target) => !target.os || !target.arch)) {
    targetErrorKey.value = "projects.targetRequired";
    return;
  }
  const seenTargets = new Set<string>();
  for (const target of targets) {
    const key = JSON.stringify([target.os, target.arch]);
    if (seenTargets.has(key)) {
      targetErrorKey.value = "projects.targetDuplicate";
      targetErrorParams.value = { os: target.os!, arch: target.arch! };
      return;
    }
    seenTargets.add(key);
  }
  savingTargets.value = true;
  targetErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, buildTargets: targets };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    targetEditorOpen.value = false;
    targetSaveSuccess.value = true;
  } catch (reason) {
    targetErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingTargets.value = false;
  }
}

function openBootstrapEditor(): void {
  if (!project.value || !canEditProject.value) return;
  bootstrapSaveSuccess.value = false;
  bootstrapErrorKey.value = "";
  editingBootstrapRepositories.value = (project.value.spec?.bootstrapRepo || []).map((repo) => ({
    name: repo.name || "",
    repo: repo.repo || "",
  }));
  if (!editingBootstrapRepositories.value.length) addBootstrapRepository();
  bootstrapEditorOpen.value = true;
}

function closeBootstrapEditor(): void {
  if (savingBootstrap.value) return;
  bootstrapEditorOpen.value = false;
  bootstrapErrorKey.value = "";
}

function addBootstrapRepository(): void {
  editingBootstrapRepositories.value.push({ name: "", repo: "" });
}

function removeBootstrapRepository(index: number): void {
  editingBootstrapRepositories.value.splice(index, 1);
}

async function saveBootstrapRepositories(): Promise<void> {
  if (!project.value) return;
  const repositories = editingBootstrapRepositories.value.map((repo) => ({ name: repo.name?.trim(), repo: repo.repo?.trim() }));
  if (repositories.some((repo) => !repo.name || !repo.repo)) {
    bootstrapErrorKey.value = "project.bootstrapRequiredFields";
    return;
  }
  savingBootstrap.value = true;
  bootstrapErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, bootstrapRepo: repositories };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    bootstrapEditorOpen.value = false;
    bootstrapSaveSuccess.value = true;
  } catch (reason) {
    bootstrapErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingBootstrap.value = false;
  }
}

function startPayloadEditing(): void {
  if (!project.value || !canEditProject.value) return;
  payloadDraft.value = project.value.spec?.buildPayload || "";
  payloadErrorKey.value = "";
  payloadSaveSuccess.value = false;
  payloadEditing.value = true;
}

function cancelPayloadEditing(): void {
  if (savingPayload.value) return;
  payloadEditing.value = false;
  payloadErrorKey.value = "";
}

async function savePayload(): Promise<void> {
  if (!project.value) return;
  savingPayload.value = true;
  payloadErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = { ...updated.spec, buildPayload: payloadDraft.value };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    payloadEditing.value = false;
    payloadSaveSuccess.value = true;
  } catch (reason) {
    payloadErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingPayload.value = false;
  }
}

function openBasicEditor(): void {
  if (!project.value || !canEditProject.value) return;
  basicDraft.value = {
    displayName: project.value.spec?.displayName || "",
    description: project.value.spec?.description || "",
    defaultRef: {
      type: project.value.spec?.defaultRef?.type || "Branch",
      value: project.value.spec?.defaultRef?.value || "master",
    },
  };
  basicErrorKey.value = "";
  basicSaveSuccess.value = false;
  basicEditorOpen.value = true;
}

function closeBasicEditor(): void {
  if (savingBasic.value) return;
  basicEditorOpen.value = false;
  basicErrorKey.value = "";
}

async function saveBasicConfig(): Promise<void> {
  if (!project.value || !canEditProject.value || savingBasic.value) return;
  const refValue = basicDraft.value.defaultRef.value.trim();
  if (!refValue) {
    basicErrorKey.value = "project.defaultRefRequired";
    return;
  }
  if (!isSafeGitRef(refValue)) {
    basicErrorKey.value = "project.invalidDefaultRef";
    return;
  }
  savingBasic.value = true;
  basicErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    updated.spec = {
      ...updated.spec,
      displayName: basicDraft.value.displayName.trim(),
      description: basicDraft.value.description.trim(),
      defaultRef: { type: basicDraft.value.defaultRef.type, value: refValue },
    };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    basicEditorOpen.value = false;
    basicSaveSuccess.value = true;
  } catch (reason) {
    basicErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingBasic.value = false;
  }
}

function openMemberEditor(): void {
  if (!project.value || !canEditProject.value) return;
  editingMembers.value = [...memberUsernames.value];
  memberDraft.value = "";
  memberErrorKey.value = "";
  memberSaveSuccess.value = false;
  memberEditorOpen.value = true;
}

function closeMemberEditor(): void {
  if (savingMembers.value) return;
  memberEditorOpen.value = false;
  memberErrorKey.value = "";
}

function addMember(): void {
  const username = memberDraft.value.trim();
  memberErrorKey.value = "";
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(username)) {
    memberErrorKey.value = "project.invalidMemberUsername";
    return;
  }
  if (username === ownerUsername.value) {
    memberErrorKey.value = "project.ownerCannotBeMember";
    return;
  }
  if (editingMembers.value.includes(username)) {
    memberErrorKey.value = "project.memberAlreadyAdded";
    return;
  }
  editingMembers.value.push(username);
  editingMembers.value.sort((left, right) => left.localeCompare(right));
  memberDraft.value = "";
}

function removeMember(index: number): void {
  editingMembers.value.splice(index, 1);
  memberErrorKey.value = "";
}

async function saveMembers(): Promise<void> {
  if (!project.value) return;
  if (memberDraft.value.trim()) {
    addMember();
    if (memberErrorKey.value) return;
  }
  savingMembers.value = true;
  memberErrorKey.value = "";
  try {
    const updated = JSON.parse(JSON.stringify(project.value)) as Project;
    const labels = { ...(updated.metadata?.labels || {}) };
    for (const key of Object.keys(labels)) {
      if (key.startsWith("ebs.io/member-user.")) delete labels[key];
    }
    for (const username of editingMembers.value) labels[`ebs.io/member-user.${username}`] = "true";
    updated.metadata = { ...updated.metadata, labels };
    project.value = await request<Project>(`/apis/ebs/v1/projects/${encodeURIComponent(name.value)}`, {
      method: "PUT",
      body: JSON.stringify(updated),
    });
    memberEditorOpen.value = false;
    memberSaveSuccess.value = true;
  } catch (reason) {
    memberErrorKey.value = errorTranslationKey(reason, "errors.updateProject");
  } finally {
    savingMembers.value = false;
  }
}

function normalizeTab(value: unknown): ProjectTab {
  return value === "builds" || value === "config" ? value : "overview";
}

function buildPaginationItems(total: number, current: number): Array<{ key: string; page: number | null }> {
  const pages = new Set<number>();
  if (total <= 7) {
    for (let page = 1; page <= total; page += 1) pages.add(page);
  } else if (current <= 4) {
    for (let page = 1; page <= 5; page += 1) pages.add(page);
    pages.add(total);
  } else if (current >= total - 3) {
    pages.add(1);
    for (let page = total - 4; page <= total; page += 1) pages.add(page);
  } else {
    pages.add(1);
    pages.add(current - 1);
    pages.add(current);
    pages.add(current + 1);
    pages.add(total);
  }
  const sorted = [...pages].sort((left, right) => left - right);
  const result: Array<{ key: string; page: number | null }> = [];
  sorted.forEach((page, index) => {
    if (index > 0 && page - sorted[index - 1] > 1) result.push({ key: `package-ellipsis-${page}`, page: null });
    result.push({ key: `package-page-${page}`, page });
  });
  return result;
}

function exportYaml(): void {
  if (!project.value) return;
  exportErrorKey.value = "";
  try {
    const yaml = stringify(project.value, { indent: 2, lineWidth: 0 });
    const blob = new Blob([yaml], { type: "application/yaml;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `${project.value.metadata?.name || "project"}.yaml`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  } catch {
    exportErrorKey.value = "project.exportFailed";
  }
}

async function copyProjectId(): Promise<void> {
  const value = project.value?.metadata?.name;
  if (!value) return;
  copyErrorKey.value = "";
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(value);
    else fallbackCopy(value);
    copiedId.value = true;
    window.clearTimeout(copyResetTimer);
    copyResetTimer = window.setTimeout(() => {
      copiedId.value = false;
    }, 1800);
  } catch {
    copyErrorKey.value = "project.copyFailed";
  }
}

function fallbackCopy(value: string): void {
  const input = document.createElement("textarea");
  input.value = value;
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.appendChild(input);
  input.select();
  const copied = document.execCommand("copy");
  input.remove();
  if (!copied) throw new Error("copy failed");
}

function resourceValue(items: unknown[]): string | number {
  return resourcesLoading.value ? t("common.emptyValue") : items.length;
}

function buildTargetLabel(build: Build): string {
  const target = build.spec?.buildTarget;
  return targetListLabel(target ? [target] : []);
}

function baseBuildLabel(build: Build): string {
  const base = build.status?.baseBuildRef;
  return base?.name || t("common.emptyValue");
}

function targetListLabel(targets?: BuildTarget[]): string {
  if (!targets?.length) return t("common.emptyValue");
  return targets.map((target) => [target.os, target.arch].filter(Boolean).join(" / ")).filter(Boolean).join("，") || t("common.emptyValue");
}

function gitRefLabel(ref?: GitRef): string {
  return [ref?.type, ref?.value].filter(Boolean).join(" / ") || t("common.emptyValue");
}

function isSafeGitRef(value: string): boolean {
  return /^[A-Za-z0-9._/][A-Za-z0-9._/-]*$/.test(value) && !/(^|\/)\.{1,2}($|\/)/.test(value) && !value.includes("..");
}

function booleanLabel(value?: boolean): string {
  return value ? t("common.yes") : t("common.no");
}

function formatDate(value?: string): string {
  if (!value) return t("common.emptyValue");
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return t("common.emptyValue");
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
</script>
