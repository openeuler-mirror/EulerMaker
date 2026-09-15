import zhCN from "@/i18n/messages/zh-CN";

type MessageShape<T> = { [K in keyof T]: T[K] extends string ? string : MessageShape<T[K]> };

export default {
  app: {
    title: "EulerMaker",
    description: "EulerMaker package build system console",
    home: "Home",
    projects: "Projects",
    login: "Sign in",
    logout: "Sign out",
    openNavigation: "Open navigation",
    mainNavigation: "Main navigation",
    switchLanguage: "切换为中文",
    footer: "openEuler package build system",
    footerLinks: "Legal and privacy links",
    privacy: "Privacy Policy",
    legal: "Legal Notice",
    cookies: "About Cookies",
  },
} as const satisfies MessageShape<typeof zhCN>;
