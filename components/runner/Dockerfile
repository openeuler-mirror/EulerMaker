FROM openeuler/openeuler:24.03-lts AS builder

RUN dnf update -y && \
    dnf install -y golang git && \
    dnf clean all

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
RUN go build -ldflags="-s -w" -o ebs-runner ./cmd/runner/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates shadow-utils bash && \
    dnf clean all

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/ebs-runner .

RUN mkdir -p /var/lib/ebs-runner /etc/ebs/certs && \
    chown -R ebs:ebs /app /var/lib/ebs-runner /etc/ebs

USER ebs

ENTRYPOINT ["/app/ebs-runner"]
