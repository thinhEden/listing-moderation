FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# `make test` builds this stage. It sits before the runtime stage so that the
# runtime stage stays the default (last) target. The Debian-based image is used
# because the race detector needs cgo and a C compiler.
FROM golang:1.26 AS test
ENV CGO_ENABLED=1
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM alpine:3.22 AS runtime
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
COPY seed /app/seed
ENV SEED_DIR=/app/seed
USER app
EXPOSE 8080
ENTRYPOINT ["/app/server"]
