FROM openeuler/openeuler:24.03-lts AS builder

RUN dnf update -y && \
    dnf install -y golang git && \
    dnf clean all

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /build

COPY go.mod ./
COPY . .
RUN go build -ldflags="-s -w" -o artifact-manager ./cmd/server/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates shadow-utils && \
    dnf clean all

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/artifact-manager .

RUN mkdir -p /var/lib/ebs-artifacts && \
    chown -R ebs:ebs /app /var/lib/ebs-artifacts

USER ebs

VOLUME ["/var/lib/ebs-artifacts"]
EXPOSE 8080

ENTRYPOINT ["/app/artifact-manager"]
