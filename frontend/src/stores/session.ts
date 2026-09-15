import { defineStore } from "pinia";

import { checkSession, login, readToken, saveToken } from "@/api";
import type { Session } from "@/types";

export const useSessionStore = defineStore("session", {
  state: () => ({ session: null as Session | null, ready: false }),
  getters: {
    authenticated: (state) => Boolean(state.session),
    username: (state) => state.session?.identity.name || "",
    role: (state) => state.session?.identity.type || "",
  },
  actions: {
    async restore(): Promise<void> {
      if (!readToken()) {
        this.ready = true;
        return;
      }
      try {
        this.session = await checkSession();
      } catch {
        saveToken("");
      } finally {
        this.ready = true;
      }
    },
    async signIn(username: string, password: string): Promise<void> {
      this.session = await login(username, password);
    },
    signOut(): void {
      saveToken("");
      this.session = null;
    },
  },
});
