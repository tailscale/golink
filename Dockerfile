FROM --platform=$BUILDPLATFORM golang:1.23-alpine as build

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

FROM gcr.io/distroless/static-debian12:nonroot

ENV HOME /home/nonroot

COPY --from=build /work/golink /golink
ENTRYPOINT ["/golink"]
CMD ["--sqlitedb", "/home/nonroot/golink.db", "--verbose"]
