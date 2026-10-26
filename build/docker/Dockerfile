ARG HD_VERSION="4.0.0-rc-ddaf102"

FROM golang:1.27.3-alpine AS build

WORKDIR /build/hd-driver-docker

COPY go.mod go.sum cmd/ ./
RUN go install -v ./...

FROM docker.io/charleshoney/honeydipper:$HD_VERSION

WORKDIR /opt/honeydipper/drivers/builtin
COPY --from=build /go/bin/* ./
