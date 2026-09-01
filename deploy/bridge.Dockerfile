# cmd/bridge imports nothing under internal/ (development-plan.md §2:
# "bridge ... Stateless" — no store, no domain, no policy) — its own
# COPY list stays narrower than control-plane.Dockerfile's on purpose.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/bridge ./cmd/bridge
RUN CGO_ENABLED=0 go build -o /out/bridge ./cmd/bridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bridge /bridge
EXPOSE 8091
USER nonroot:nonroot
ENTRYPOINT ["/bridge"]
