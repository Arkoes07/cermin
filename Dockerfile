FROM golang:1.26.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /cermin ./cmd/cermin

# Litestream's image is Debian slim with CA certificates, sqlite3 and
# litestream in /usr/local/bin.
FROM litestream/litestream:0.5.17
COPY --from=build /cermin /usr/local/bin/cermin
COPY litestream.yml /etc/litestream.yml
# Restores the database from R2 when the volume has none and R2 has a copy,
# then runs cermin, replicating the database to R2 for as long as it runs.
ENTRYPOINT ["litestream", "replicate", "-restore-if-db-not-exists", "-exec", "cermin serve"]
