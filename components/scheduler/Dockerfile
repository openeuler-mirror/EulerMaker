FROM golang:1.21 AS builder

ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /build/components/scheduler
COPY api/go.mod api/go.sum /build/api/
COPY components/scheduler/go.mod components/scheduler/go.sum ./
RUN go mod download

COPY api /build/api
COPY components/scheduler .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /scheduler ./cmd/scheduler

FROM openeuler/openeuler:24.03-lts
RUN dnf install -y ca-certificates shadow-utils && dnf clean all && groupadd scheduler && useradd -g scheduler -s /sbin/nologin scheduler
COPY --from=builder /scheduler /usr/local/bin/ebs-scheduler
USER scheduler
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ebs-scheduler"]
