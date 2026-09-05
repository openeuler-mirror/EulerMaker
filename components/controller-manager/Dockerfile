FROM golang:1.21 AS builder

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /build/components/controller-manager
COPY api/go.mod api/go.sum /build/api/
COPY components/controller-manager/go.mod components/controller-manager/go.sum ./
RUN go mod download
COPY api /build/api
COPY components/controller-manager .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /controller-manager ./cmd/controller-manager

FROM openeuler/openeuler:24.03-lts
RUN dnf install -y ca-certificates shadow-utils && dnf clean all && groupadd controller-manager && useradd -g controller-manager -s /sbin/nologin controller-manager
COPY --from=builder /controller-manager /usr/local/bin/ebs-controller-manager
USER controller-manager
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ebs-controller-manager"]
