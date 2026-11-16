# One builder stage builds both binaries. Pick the image with --target:
#   docker build --target tokenwarden .
#   docker build --target mockanthropic .

# golang:1.27.4-alpine
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags='-s -w' -o /out/ ./cmd/tokenwarden ./cmd/mockanthropic

# gcr.io/distroless/static-debian13:nonroot
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS base
USER nonroot:nonroot

FROM base AS mockanthropic
COPY --from=build /out/mockanthropic /usr/local/bin/mockanthropic
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/mockanthropic", "-addr", "0.0.0.0:8081"]

FROM base AS tokenwarden
COPY --from=build /out/tokenwarden /usr/local/bin/tokenwarden
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/tokenwarden", "-addr", "0.0.0.0:8080", "-metrics-addr", "0.0.0.0:9090"]
