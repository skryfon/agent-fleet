FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/supervisor ./cmd/supervisor
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/supervisor ./cmd/supervisor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/supervisor /supervisor
EXPOSE 8090
USER nonroot:nonroot
ENTRYPOINT ["/supervisor"]
