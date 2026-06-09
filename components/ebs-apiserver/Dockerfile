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
RUN go mod tidy && go build -ldflags="-s -w" -o ebs-apiserver ./cmd/server/

FROM openeuler/openeuler:24.03-lts

RUN dnf update -y && \
    dnf install -y ca-certificates shadow-utils && \
    dnf clean all

RUN groupadd ebs && useradd -g ebs -s /sbin/nologin ebs

WORKDIR /app

COPY --from=builder /build/ebs-apiserver .

RUN mkdir -p /etc/ebs/certs && \
    chown -R ebs:ebs /app /etc/ebs

USER ebs

EXPOSE 8443

ENTRYPOINT ["/app/ebs-apiserver"]