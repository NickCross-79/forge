# Container image for forge.
#
# This is entirely optional: forge is a single static binary and needs no
# container to run. The image exists for people who would rather not install a
# Go toolchain, and for the docker-compose development loop.

# --- build the dashboard ---------------------------------------------------
FROM node:22-alpine AS web

WORKDIR /src/web
# Copy manifests first so a dependency install is cached across source edits.
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund

COPY web/ ./
RUN npm run build

# --- build the binary ------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The dashboard bundle has to be in place before the Go build, because it is
# embedded into the binary.
COPY --from=web /src/web/dist ./web/dist

ARG VERSION=docker
# CGO stays off: the SQLite driver is pure Go, so this is a static binary with
# no C runtime to carry into the final image.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/nickcross-79/forge/internal/cli.buildVersion=${VERSION}" \
      -o /out/forge ./cmd/forge

# --- runtime ---------------------------------------------------------------
FROM alpine:3.20

# git and a shell are here because pipelines routinely need them; ca-certificates
# so jobs can reach HTTPS endpoints.
RUN apk add --no-cache ca-certificates git tini \
    && adduser -D -u 10001 forge

COPY --from=build /out/forge /usr/local/bin/forge

# The project being built is mounted here.
WORKDIR /workspace
RUN chown forge:forge /workspace

USER forge

EXPOSE 7777

# tini reaps the processes that pipeline jobs leave behind, so a container
# running long pipelines does not accumulate zombies.
ENTRYPOINT ["/sbin/tini", "--", "forge"]
CMD ["serve", "--host", "0.0.0.0"]
