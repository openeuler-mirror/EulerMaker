import { createI18n } from "vue-i18n";

import enUS from "@/i18n/messages/en-US";
import zhCN from "@/i18n/messages/zh-CN";

export type Locale = "zh-CN" | "en-US";

export const LOCALE_KEY = "eulermaker.locale";

export function initialLocale(): Locale {
  const saved = localStorage.getItem(LOCALE_KEY);
  if (saved === "zh-CN" || saved === "en-US") return saved;
  return navigator.language.toLowerCase().startsWith("zh") ? "zh-CN" : "en-US";
}

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale(),
  fallbackLocale: "zh-CN",
  messages: { "zh-CN": zhCN, "en-US": enUS },
});
