FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/payment-api ./cmd/payment-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/payment-api /usr/local/bin/payment-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/payment-api"]
