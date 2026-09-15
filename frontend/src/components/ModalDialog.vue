<template>
  <Teleport to="body">
    <div class="modal-backdrop" @mousedown.self="emit('close')">
      <section class="modal-card" role="dialog" aria-modal="true" :aria-labelledby="titleId">
        <header class="modal-heading">
          <div>
            <span v-if="eyebrow" class="eyebrow">{{ eyebrow }}</span>
            <h2 :id="titleId">{{ title }}</h2>
          </div>
          <button class="modal-close" type="button" :aria-label="closeLabel" @click="emit('close')"><Close /></button>
        </header>
        <slot />
      </section>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { Close } from "@element-plus/icons-vue";
import { onBeforeUnmount, onMounted } from "vue";

defineProps<{ title: string; eyebrow?: string; closeLabel: string; titleId: string }>();
const emit = defineEmits<{ close: [] }>();

function onKeydown(event: KeyboardEvent): void {
  if (event.key === "Escape") emit("close");
}

onMounted(() => {
  document.addEventListener("keydown", onKeydown);
  document.body.classList.add("modal-open");
});
onBeforeUnmount(() => {
  document.removeEventListener("keydown", onKeydown);
  document.body.classList.remove("modal-open");
});
</script>
