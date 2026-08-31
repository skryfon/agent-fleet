FROM golang:1.26-alpine AS build
WORKDIR /src
# go.sum must be copied before `go mod download` — the previous Dockerfile
# copied go.mod alone and never ran `go mod download`, which worked only by
# accident while go.mod had zero dependencies (M0/M1). M2 adds real deps
# (pgx, uuid, jsonschema, yaml); this layer is what keeps working.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/control-plane ./cmd/control-plane
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /control-plane
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/control-plane"]
