FROM golang:1.24 AS builder

ARG TARGETPLATFORM=linux/amd64
ARG BUILDPLATFORM=linux/amd64

# These should change very infrequently and are coupled tightly
ARG CMETRICS_VERSION=1.0.2
ENV CMETRICS_VERSION=${CMETRICS_VERSION}
ARG CMETRICS_RELEASE=v1.0.2
ENV CMETRICS_RELEASE=${CMETRICS_RELEASE}

ARG PACKAGEARCH=amd64
ENV PACKAGEARCH=${PACKAGEARCH}

WORKDIR /fluent-bit-go

COPY go.mod .
COPY go.sum .

RUN go mod download
RUN go mod verify

COPY . .

ADD https://github.com/fluent/cmetrics/releases/download/${CMETRICS_RELEASE}/cmetrics_${CMETRICS_VERSION}_${PACKAGEARCH}-headers.deb external/
ADD https://github.com/fluent/cmetrics/releases/download/${CMETRICS_RELEASE}/cmetrics_${CMETRICS_VERSION}_${PACKAGEARCH}.deb external/
RUN dpkg -i external/*.deb

RUN go build -trimpath -buildmode=c-shared -o /fluent-bit-go/custom_jaeger_remote.so /fluent-bit-go/*.go

FROM ghcr.io/chronosphereio/calyptia-internal/core-fluent-bit:main

COPY --from=builder /fluent-bit-go/custom_jaeger_remote.so /fluent-bit/etc/
COPY --from=builder /fluent-bit-go/fluent-bit.conf /fluent-bit/etc/
COPY --from=builder /fluent-bit-go/plugins.conf /fluent-bit/etc/

ENTRYPOINT [ "/fluent-bit/bin/calyptia-fluent-bit" ]
CMD [ "/fluent-bit/bin/calyptia-fluent-bit", "-c", "/fluent-bit/etc/fluent-bit.conf" ]
