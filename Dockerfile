FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/zulip-gcal-service . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/zulip-gcal-service /zulip-gcal-service
# A fresh named volume inherits this ownership, so the nonroot user can write it.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 8080
# No HEALTHCHECK: distroless has no shell; docker-compose.yml runs
# `/zulip-gcal-service healthcheck` instead.
ENTRYPOINT ["/zulip-gcal-service"]
