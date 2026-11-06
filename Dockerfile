# Static linux binary. The 103-part SQLite file is copied to /app/data and is
# also embedded in the binary. SMD_DB defaults to ./data/smd_parts.db, which
# is that file. An existing database is never overwritten, and seed/parts.json
# is never imported. The container filesystem is ephemeral.
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
COPY data /app/data
EXPOSE 8080
ENV PORT=8080 \
    SMD_OPEN_BROWSER=0
ENTRYPOINT ["/app/smd-parts"]
