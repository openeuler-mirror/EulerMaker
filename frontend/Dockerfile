FROM node:22-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

FROM nginx:1.27-alpine

ENV DEFAULT_ARTIFACT_MANAGER=artifact-manager:8081 \
    NGINX_ENVSUBST_FILTER="DEFAULT_ARTIFACT_MANAGER|NGINX_DNS_RESOLVER"

COPY --chmod=755 deploy/10-resolver.envsh /docker-entrypoint.d/10-resolver.envsh
COPY deploy/nginx.conf /etc/nginx/templates/default.conf.template
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1
