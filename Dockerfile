FROM golang:1.23 AS build

WORKDIR /src

COPY golang/go.mod golang/go.sum ./
RUN go mod download

COPY golang/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/k8s-sre-tool .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/k8s-sre-tool /k8s-sre-tool

EXPOSE 8080

ENTRYPOINT ["/k8s-sre-tool"]
