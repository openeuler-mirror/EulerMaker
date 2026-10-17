FROM golang:1.27.2-bookworm AS builder

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /build/backend/ebs-gateway

COPY api/go.mod api/go.sum /build/api/
COPY backend/ebs-gateway/go.mod backend/ebs-gateway/go.sum ./
RUN go mod download

COPY api /build/api
COPY backend/ebs-gateway .
RUN go build -ldflags="-s -w" -o ebs-gateway ./cmd/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates shadow-utils && \
    dnf clean all

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/backend/ebs-gateway/ebs-gateway .

RUN mkdir -p /etc/ebs/certs && \
    chown -R ebs:ebs /app /etc/ebs

USER ebs

EXPOSE 8080

ENTRYPOINT ["/app/ebs-gateway"]
