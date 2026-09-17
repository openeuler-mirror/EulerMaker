import { computed, onMounted, ref } from "vue";
import { errorTranslationKey, request } from "@/api";
import type { BuildConf, BuildTarget } from "@/types";
import { configuredOS, configuredArches, supportsTarget } from "@/components/buildConfDraft";

const configuration = ref<BuildConf | null>(null);
const loading = ref(false);
const error = ref("");
let pending: Promise<void> | null = null;

export function useBuildConf() {
  async function reload(): Promise<void> {
    if (pending) return pending;
    loading.value = true;
    error.value = "";
    pending = (async () => {
      try { configuration.value = await request<BuildConf>("/apis/ebs/v1/buildconfs/default"); }
      catch (reason) { configuration.value = null; error.value = errorTranslationKey(reason, "buildConf.loadFailed"); }
      finally { loading.value = false; pending = null; }
    })();
    return pending;
  }
  const operatingSystems = computed(() => configuredOS(configuration.value));
  const arches = (os?: string) => configuredArches(configuration.value, os);
  const supports = (target: BuildTarget) => !loading.value && !error.value && supportsTarget(configuration.value, target);
  onMounted(() => { void reload(); });
  return { configuration, loading, error, reload, operatingSystems, arches, supports };
}
