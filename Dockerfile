FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata wget && \
    adduser -D -s /bin/sh olla && \
    mkdir -p /app/logs && \
    chown -R olla:olla /app

WORKDIR /app

# Copy each runtime file by name rather than COPY . . - this Dockerfile is built
# from two different contexts (the repo root via make docker-build-local, and
# goreleaser's synthesised extra_files context), and naming them keeps the two
# images identical instead of relying on .dockerignore to filter one of them.
COPY olla /usr/local/bin/olla
# config/config.yaml, not the root config.yaml: the loader searches
# config/config.yaml first (internal/config/config.go), so in a repo-root context
# the bare-metal config would win and bind the server to loopback.
COPY build/docker-config.yaml config/config.yaml
COPY config/models.yaml config/models.yaml
COPY config/profiles/ config/profiles/

RUN chown -R olla:olla /app

USER olla

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:40114/internal/health || exit 1

EXPOSE 40114
ENTRYPOINT ["olla"]
