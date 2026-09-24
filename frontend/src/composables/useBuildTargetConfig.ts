import { computed, onMounted, ref } from "vue";
import { errorTranslationKey, request } from "@/api";
import type { BuildTargetContent, BuildTarget } from "@/types";
import type { Config } from "@/types";
import { parse } from "yaml";
import { parseBuildTargetContent, configuredOS, configuredArches, supportsTarget } from "@/components/buildTargetConfigDraft";

const configuration = ref<BuildTargetContent | null>(null);
const loading = ref(false);
const error = ref("");
let pending: Promise<void> | null = null;

export function useBuildTargetConfig() {
  async function reload(): Promise<void> {
    if (pending) return pending;
    loading.value = true;
    error.value = "";
    pending = (async () => {
      try {
        const config = await request<Config>("/apis/ebs/v1/configs/build-target");
        if (config.metadata?.name !== "build-target") throw new Error("invalid build-target Config");
        configuration.value = parseBuildTargetContent(parse(config.spec.content, { uniqueKeys: true }));
      }
      catch (reason) { configuration.value = null; error.value = errorTranslationKey(reason, "buildTargetConfig.loadFailed"); }
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
