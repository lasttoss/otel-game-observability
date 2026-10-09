# The build stage has a Go toolchain and the runtime stage does not: a game server binary is 15MB, and a
# 300MB image is 300MB of CVE surface nobody needs in a container that serves UDP.
# The toolchain has to satisfy the go directive in go.mod, so this follows the latest stable rather than
# pinning a minor that will drift out of range. CI pins with go-version-file: go.mod instead.
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gameserver ./cmd/gameserver

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/gameserver /usr/local/bin/gameserver
USER 10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gameserver"]
