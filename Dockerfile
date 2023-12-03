FROM golang:1.21.4-alpine

WORKDIR /app

RUN apk add --no-cache \
    unzip \
    ca-certificates \
    # this is needed only if you want to use scp to copy later your pb_data locally
    openssh

# copy go.mod and download dependencies
COPY go.mod ./
COPY go.sum ./
RUN go mod download

# copy the rest of the files and build
COPY *.go ./
RUN go build -o /pocketbase .

EXPOSE 8080

# start PocketBase
CMD ["/pocketbase", "serve", "--http=0.0.0.0:8080"]
