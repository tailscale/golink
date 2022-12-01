FROM cgr.dev/chainguard/go:1.19 as build

WORKDIR /work

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -v ./cmd/golink


FROM cgr.dev/chainguard/static:latest

ENV HOME /home/nonroot

COPY --from=build /work/golink /golink
ENTRYPOINT ["/golink"]
CMD ["--sqlitedb", "/home/nonroot/golink.db", "--verbose"]
