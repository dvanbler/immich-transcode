# immich-transcode with Debian's ffmpeg (libx265) and exiftool.
#
#   docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t immich-transcode .
#
# The Go binary is cross-compiled on the build platform; only the runtime
# stage runs under emulation for other architectures.

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/immich-transcode .

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg libimage-exiftool-perl ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home app
COPY --from=build /out/immich-transcode /usr/local/bin/immich-transcode
# The config file is read from /config/immich-transcode/config.yaml.
ENV XDG_CONFIG_HOME=/config
USER app
ENTRYPOINT ["immich-transcode"]
CMD ["--help"]
