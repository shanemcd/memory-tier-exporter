FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS build
RUN microdnf install -y go git && microdnf clean all
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /memory-tier-exporter ./cmd/memory-tier-exporter && \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /memory-hog ./cmd/memory-hog

FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY --from=build /memory-tier-exporter /usr/local/bin/memory-tier-exporter
COPY --from=build /memory-hog /usr/local/bin/memory-hog
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/memory-tier-exporter"]
