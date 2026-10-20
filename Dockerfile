FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/cipherbase-server ./cmd/server

FROM alpine:3.20
COPY --from=build /out/cipherbase-server /usr/local/bin/cipherbase-server
EXPOSE 8090
VOLUME /data
CMD ["cipherbase-server", "--dir", "/data", "--addr", ":8090"]
