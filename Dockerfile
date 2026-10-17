# The controller image: one static Go binary, nothing else. It holds no secrets; the GitHub App key,
# the webhook secret and the Railway token arrive as environment variables at run time.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /controller /controller
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/controller"]
