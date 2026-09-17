<template>
  <section class="page-heading">
    <h1>{{ t("app.userManagement") }}</h1>
  </section>
  <nav class="project-tabs admin-tabs" :aria-label="t('app.userManagement')">
    <RouterLink to="/admin/users">{{ t("admin.usersTitle") }}</RouterLink>
    <RouterLink to="/admin/machineaccounts">{{ t("admin.machineAccounts") }}</RouterLink>
  </nav>
  <RouterView />
</template>

<script setup lang="ts">
import { RouterLink, RouterView } from "vue-router";
import { useI18n } from "vue-i18n";
const { t } = useI18n();
</script>

<style scoped>
.admin-tabs { margin: 0 0 24px; }
.admin-tabs a { position: relative; padding: 13px 2px 14px; color: #59677b; font-size: 15px; font-weight: 650; white-space: nowrap; text-decoration: none; }
.admin-tabs a:hover, .admin-tabs a.router-link-active { color: var(--blue); }
.admin-tabs a.router-link-active::after { content: ""; position: absolute; right: 0; bottom: -1px; left: 0; height: 3px; border-radius: 3px 3px 0 0; background: var(--blue); }
.admin-tabs a:focus-visible { outline: 2px solid #9bc5f5; outline-offset: 2px; border-radius: 3px; }
</style>
