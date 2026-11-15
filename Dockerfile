# One static binary, ygg, holding every part. Each container runs one part:
# `command: [gjallar]`. Parts that must see the host (Veðrfölnir, Ratatoskr)
# run the same binary as systemd user units instead (deploy/systemd).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X yggdrasil/internal/part.Version=${VERSION}" -o /ygg ./cmd/ygg

FROM alpine:3.22
COPY --from=build /ygg /usr/local/bin/ygg
USER 1000:1000
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/ygg"]
