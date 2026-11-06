# Static linux binary. The live SQLite file is copied to /seed/data and is also
# embedded in the binary. If SMD_DB is missing, that seed file is copied once.
# An existing database is never overwritten, and seed/parts.json is never imported.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/smd-parts .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/smd-parts /app/smd-parts
COPY data /seed/data
EXPOSE 8080
ENV PORT=8080 \
    SMD_DB=/data/smd_parts.db \
    SMD_SEED_DB=/seed/data/smd_parts.db \
    SMD_OPEN_BROWSER=0
ENTRYPOINT ["/app/smd-parts"]
