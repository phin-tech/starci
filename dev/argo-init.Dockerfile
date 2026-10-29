# The image for `starci compile --target argo --init-image`: what the init
# container of each pod needs to put a step's inputs and tools in place (git,
# curl, tar, unzip), and starci, to ask the shared store whether a step has
# passed and to record that it has. Build and push it where the cluster can
# pull it, and name it by digest:
#
#   docker build -f dev/argo-init.Dockerfile -t REGISTRY/starci-init .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /starci ./cmd/starci

FROM alpine:3.22
RUN apk add --no-cache git curl tar unzip ca-certificates
COPY --from=build /starci /usr/local/bin/starci
