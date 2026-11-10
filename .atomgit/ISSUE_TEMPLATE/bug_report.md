---
name: "问题反馈 / Bug report"
about: "报告构建、部署、页面或接口异常 / Report build, deployment, UI, or API issues"
title: "[Bug] "
---

<!-- 请先搜索已有 Issue，避免重复提交。可以使用中文或英文填写，不适用的项目可删除。
请勿提交密码、Token、Cookie、API Key、私钥或未脱敏的配置、日志。
涉及安全漏洞时，请先联系维护者确认私密报告渠道，不要在公开 Issue 中披露利用细节。
Please search existing issues to avoid duplicates. Reports in Chinese or English are welcome; remove fields that do not apply.
Do not include passwords, tokens, cookies, API keys, private keys, or configuration and logs containing sensitive information.
For security vulnerabilities, contact the maintainers to confirm a private reporting channel before disclosing exploit details in a public issue.
-->

## 问题描述 / Description

<!-- 描述发生了什么、影响范围，以及是否持续发生或偶发。
Describe what happened, the scope of impact, and whether the issue is persistent or intermittent.
-->

## 复现步骤 / Steps to reproduce

1.
2.
3.

## 预期结果 / Expected behavior

## 实际结果 / Actual behavior

## 环境信息 / Environment

<!-- 仅填写相关项。使用 dev/latest 镜像时，请尽量提供镜像 digest、Git commit 或 Runner Agent 版本。
Fill in only relevant fields. For dev/latest images, provide the image digest, Git commit, or Runner Agent version if available.
-->

- EulerMaker 版本或 Git commit / EulerMaker version or Git commit:
- 镜像 digest 或 Runner Agent 版本 / Image digest or Runner Agent version:
- 受影响组件 / Affected component (Frontend, Gateway, apiserver, Controller Manager, Scheduler, Runner, Git Server, Artifact Manager, ebsctl):
- 部署方式 / Deployment (社区环境 / Community instance, Docker Compose, Kubernetes, 其他 / Other):
- 操作系统与架构 / Operating system and architecture:
- 容器运行时及版本（Runner 问题） / Container runtime and version (Docker / containerd, for Runner issues):
- 浏览器及版本（页面问题） / Browser and version (for UI issues):

## 定位信息 / Diagnostics

<!-- 按需提供发生时间及所在时区、Project / Build / Job 名称、phase / stage。
接口问题请提供请求方法、路径、HTTP 状态码和脱敏响应，不要粘贴完整认证请求头。
上传或签名问题请同时提供 Runner 与 Artifact Manager 的相关错误。
Where relevant, provide the occurrence time and time zone, Project / Build / Job names, and phase / stage.
For API issues, include the request method, path, HTTP status code, and sanitized response. Do not paste full authentication headers.
For upload or signing issues, include relevant errors from both Runner and Artifact Manager.
-->

- 发生时间及所在时区 / Occurrence time and time zone:
- 相关对象或请求 / Related resources or requests:

```text
粘贴脱敏后的错误日志或响应；较长日志请作为附件上传。
Paste sanitized error logs or responses here; upload longer logs as attachments.
```

## 补充信息 / Additional context

<!-- 可补充截图、最近的升级或配置变更、已尝试的排查方法及结果。
Optionally include screenshots, recent upgrades or configuration changes, and troubleshooting steps already tried with their results.
-->
