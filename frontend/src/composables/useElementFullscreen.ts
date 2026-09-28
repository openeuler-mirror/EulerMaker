import { onBeforeUnmount, onMounted, ref } from 'vue';

export function useElementFullscreen() {
  const element = ref<HTMLElement | null>(null);
  const isFullscreen = ref(false);

  function syncFullscreen(): void {
    isFullscreen.value = document.fullscreenElement === element.value;
  }

  async function toggleFullscreen(): Promise<void> {
    if (!element.value) return;
    try {
      if (isFullscreen.value) await document.exitFullscreen();
      else await element.value.requestFullscreen();
    } catch {
      syncFullscreen();
    }
  }

  onMounted(() => document.addEventListener('fullscreenchange', syncFullscreen));
  onBeforeUnmount(() => document.removeEventListener('fullscreenchange', syncFullscreen));

  return { element, isFullscreen, toggleFullscreen };
}
