# The controller image: one static Go binary, nothing else. It holds no secrets; the GitHub App key,
# the webhook secret and the Railway token arrive as environment variables at run time.
# base images are pinned by digest as well as tag (golang 1.24-alpine, distroless static-debian12 nonroot)
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /controller /controller
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/controller"]
