# One image for every role: `starci worker`, `starci serve`, or a one-off
# `starci run`. It carries git and the docker client because a worker drives
# both; the docker daemon it talks to is somewhere else.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /starci ./cmd/starci

FROM docker:28-cli
RUN apk add --no-cache git ca-certificates tar
COPY --from=build /starci /usr/local/bin/starci
ENTRYPOINT ["starci"]
