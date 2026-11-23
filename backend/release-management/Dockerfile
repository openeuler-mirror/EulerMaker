FROM golang:1.27.2-bookworm AS builder

ENV GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0
WORKDIR /build/backend/release-management
COPY api /build/api
COPY backend/release-management/go.mod backend/release-management/go.sum ./
RUN go mod download
COPY backend/release-management .
RUN go build -ldflags="-s -w" -o release-management ./cmd

FROM openeuler/openeuler:24.03-lts
RUN dnf install -y ca-certificates git shadow-utils && dnf clean all && \
    groupadd -g 1000 ebs && useradd -u 1000 -g ebs -s /sbin/nologin ebs && \
    mkdir -p /var/lib/release-management && chown ebs:ebs /var/lib/release-management
COPY --from=builder /build/backend/release-management/release-management /usr/local/bin/release-management
USER ebs
VOLUME ["/var/lib/release-management"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/release-management"]
