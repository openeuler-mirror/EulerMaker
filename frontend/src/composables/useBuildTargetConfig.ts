import { computed, onMounted, ref } from "vue";
import { errorTranslationKey, request } from "@/api";
import type { ConfiguredBuildTarget } from "@/types";

interface BuildTarget {
  distro: string;
  arches: string[];
}

const targets = ref<BuildTarget[]>([]);
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
        const { items } = await request<{ items: BuildTarget[] }>("/apis/ebs/v1/build-targets");
        targets.value = items;
      }
      catch (reason) { targets.value = []; error.value = errorTranslationKey(reason, "buildTargetConfig.loadFailed"); }
      finally { loading.value = false; pending = null; }
    })();
    return pending;
  }
  const operatingSystems = computed(() => targets.value.map(target => target.distro));
  const arches = (os?: string) => targets.value.find(target => target.distro === os)?.arches ?? [];
  const supports = (target: ConfiguredBuildTarget) => !loading.value && !error.value && target.arch !== undefined && arches(target.os).includes(target.arch);
  onMounted(() => { void reload(); });
  return { loading, error, reload, operatingSystems, arches, supports };
}
