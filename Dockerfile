FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/wolfi-base as build
RUN apk update && apk add build-base git openssh go-1.20

WORKDIR /work

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN \
    if [ "${TARGETARCH}" = "arm" ] && [ -n "${TARGETVARIANT}" ]; then \
      export GOARM="${TARGETVARIANT#v}"; \
    fi; \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 go build -v ./cmd/golink


FROM cgr.dev/chainguard/static:latest

ENV HOME /home/nonroot

COPY --from=build /work/golink /golink
ENTRYPOINT ["/golink"]
CMD ["--sqlitedb", "/home/nonroot/golink.db", "--verbose"]
