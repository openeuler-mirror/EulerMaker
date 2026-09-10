FROM openeuler/openeuler:24.03-lts AS builder

RUN dnf update -y && \
    dnf install -y golang git && \
    dnf clean all

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o git-server ./cmd/git-server

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates curl git git-daemon openssh-clients shadow-utils && \
    dnf clean all && \
    groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app
COPY --from=builder /build/git-server /app/git-server
RUN mkdir -p /var/lib/ebs-git && chown -R ebs:ebs /app /var/lib/ebs-git

USER ebs
VOLUME ["/var/lib/ebs-git"]
EXPOSE 8080 9418
ENTRYPOINT ["/app/git-server"]
