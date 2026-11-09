import { defineStore } from "pinia";
import { oauthRequest, type LoginMethods } from "@/oauth";

let pending: Promise<void> | null = null;
export const useLoginMethodsStore = defineStore("loginMethods", {
  state: () => ({ methods: null as LoginMethods | null, loading: false, failed: false }),
  actions: {
    async load(): Promise<void> {
      if (pending) return pending;
      if (this.methods) return;
      this.loading = true;
      this.failed = false;
      pending = oauthRequest<LoginMethods>("/auth/providers")
        .then(value => { this.methods = value; })
        .catch(() => { this.failed = true; })
        .finally(() => { this.loading = false; pending = null; });
      return pending;
    },
  },
});
