FROM golang:1.25.13-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown
ARG BUILD_COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
    -X main.buildVersion=${BUILD_VERSION} \
    -X main.buildDate=${BUILD_DATE} \
    -X main.buildCommit=${BUILD_COMMIT}" \
    -o /out/meetnote ./cmd/meetnote

FROM alpine:3.21
RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app \
    && mkdir -p /data/uploads && chown -R app:app /data
COPY --from=build /out/meetnote /app/meetnote
COPY samples /app/samples
USER app
WORKDIR /app
ENV MEETNOTE_STORAGE_DIR=/data/uploads
ENTRYPOINT ["/app/meetnote"]
CMD ["bot"]
