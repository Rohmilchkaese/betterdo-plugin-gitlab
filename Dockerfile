# Multi-stage build for the BetterDo reference plugin.
#
# Final image: distroless static (~5 MB), nonroot user, no shell.
# Plugins are containerized HTTP servers per docs/plugin-contract.md §1 —
# no shell, no init system, just the binary.

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/plugin -ldflags='-s -w' .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plugin /plugin
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/plugin"]
