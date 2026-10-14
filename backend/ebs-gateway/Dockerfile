FROM golang:1.27.2-bookworm AS builder

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags="-s -w" -o ebs-gateway ./cmd/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates shadow-utils && \
    dnf clean all

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/ebs-gateway .

RUN mkdir -p /etc/ebs/certs && \
    chown -R ebs:ebs /app /etc/ebs

USER ebs

EXPOSE 8080

ENTRYPOINT ["/app/ebs-gateway"]
