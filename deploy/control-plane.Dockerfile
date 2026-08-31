FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/control-plane ./cmd/control-plane
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/control-plane /control-plane
EXPOSE 8080
ENTRYPOINT ["/control-plane"]
