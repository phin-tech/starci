# The image for `starci compile --target argo --init-image`: starci, which runs
# as each pod's init container (`starci pod prepare`) to put a step's inputs,
# artifacts and tools in place and ask the shared store whether it passed
# before, and what it calls on: git, and tar for tools that are archives.
# Build and push it where the cluster can pull it, and name it by digest:
#
#   docker build -f dev/argo-init.Dockerfile -t REGISTRY/starci-init .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /starci ./cmd/starci

FROM alpine:3.22
RUN apk add --no-cache git tar ca-certificates
COPY --from=build /starci /usr/local/bin/starci
