# Build Caddy with the zen_free_tier handler, then copy only the binary into
# the runtime image. ZEN_SESSION_ID is a secret: pass it at run time with -e,
# never put it in the image.
ARG CADDY_RELEASE=2.11.7

FROM docker.io/library/caddy:${CADDY_RELEASE}-builder-alpine AS builder
ARG CADDY_RELEASE
COPY zenfreetier /src/zenfreetier
RUN xcaddy build "v${CADDY_RELEASE}" \
    --with github.com/koosty/oc-caddy/zenfreetier=/src/zenfreetier \
    --output /out/caddy

FROM docker.io/library/caddy:${CADDY_RELEASE}-alpine
COPY --from=builder /out/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 80
