# syntax=docker/dockerfile:1

FROM golang:1.26 AS builder
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux

# Релизный бинарь goose линкуется с libc, которой в distroless/static нет:
# собираем свой, статикой и без sqlite.
RUN GOBIN=/out go install -tags='no_sqlite3' -ldflags='-s -w' \
        github.com/pressly/goose/v3/cmd/goose@v3.27.3

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VCS_REF=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VCS_REF}" -o /out/intake ./cmd/intake

FROM gcr.io/distroless/static:nonroot
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision=$VCS_REF
COPY --from=builder /out/intake /app
COPY --from=builder /out/goose /goose
COPY --from=builder /src/migrations /migrations
CMD ["/app"]
