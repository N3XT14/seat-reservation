FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /burst .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /burst /burst
ENTRYPOINT ["/burst"]
