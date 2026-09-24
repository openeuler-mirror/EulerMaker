import { computed, onMounted, ref } from "vue";
import { errorTranslationKey, request } from "@/api";
import type { BuildConf, BuildTarget } from "@/types";
import type { Config } from "@/types";
import { parse } from "yaml";
import { buildConfSpec, configuredOS, configuredArches, supportsTarget } from "@/components/buildConfDraft";

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
      try {
        const config = await request<Config>("/apis/ebs/v1/configs/build-target");
        if (config.metadata?.name !== "build-target") throw new Error("invalid build-target Config");
        configuration.value = { spec: buildConfSpec(parse(config.spec.content, { uniqueKeys: true })) };
      }
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
