import { fileURLToPath, URL } from "node:url";

import vue from "@vitejs/plugin-vue";
import { defineConfig, loadEnv } from "vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const gateway = env.VITE_EULERMAKER_GATEWAY || "http://localhost:8080";
  const artifactManager = env.VITE_EULERMAKER_ARTIFACT_MANAGER || "http://localhost:8081";

  return {
    plugins: [vue()],
    build: {
      rollupOptions: {
        input: {
          app: fileURLToPath(new URL("./index.html", import.meta.url)),
          apiDocs: fileURLToPath(new URL("./api-docs-viewer.html", import.meta.url)),
        },
      },
    },
    resolve: {
      alias: {
        "@": fileURLToPath(new URL("./src", import.meta.url)),
      },
    },
    server: {
      port: 5173,
      proxy: {
        "^/openapi\\.json(?:\\?|$)": { target: gateway, changeOrigin: true },
        "/apis": { target: gateway, changeOrigin: true },
        "/auth": { target: gateway, changeOrigin: true },
        "/artifacts": { target: artifactManager, changeOrigin: true },
        "/repositories": { target: artifactManager, changeOrigin: true },
      },
    },
  };
});
