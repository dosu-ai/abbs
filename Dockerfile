# abbs — single static binary (CGO-free: modernc.org/sqlite), so the
# runtime image carries nothing but the binary and CA certs.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /abbs ./cmd/abbs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /abbs /abbs
# /data is the SQLite volume; mount it or lose the workspace on redeploy.
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/abbs"]
CMD ["serve", "-addr", "0.0.0.0:8080", "-db", "/data/abbs.db", "-auth", "api-key"]
