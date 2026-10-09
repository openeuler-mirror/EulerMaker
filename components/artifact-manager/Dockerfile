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
RUN go build -ldflags="-s -w" -o artifact-manager ./cmd/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates createrepo_c rpm rpm-sign gnupg2 shadow-utils tar curl && \
    dnf clean all

# Install the glibc Signatrust client for the image architecture.
RUN set -eu; \
    arch="$(uname -m)"; \
    case "$arch" in \
        x86_64) asset=client-glibc-x86-64.tar.gz; sha256=3f0da598ad450fd250212be0ee4f94b59a93682d6ab74a258563768d86e84ab1 ;; \
        aarch64) asset=client-glibc-aarch64.tar.gz; sha256=490a86dfa734ed8635d13d4e056cc21c225b77077c7db33616879002adca2f1b ;; \
        *) echo "unsupported Signatrust client architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://atomgit.com/openeuler/signatrust/releases/download/v1.0.1.rc0/${asset}" -o /tmp/signatrust-client.tar.gz; \
    printf '%s  %s\n' "$sha256" /tmp/signatrust-client.tar.gz | sha256sum -c -; \
    tar -xzf /tmp/signatrust-client.tar.gz -C /tmp signatrust_client; \
    install -m 0755 /tmp/signatrust_client /usr/bin/client; \
    rm /tmp/signatrust-client.tar.gz /tmp/signatrust_client

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/artifact-manager .

RUN mkdir -p /var/lib/ebs-artifacts && \
    chown -R ebs:ebs /app /var/lib/ebs-artifacts

USER ebs

VOLUME ["/var/lib/ebs-artifacts"]
EXPOSE 8081

ENTRYPOINT ["/app/artifact-manager"]
