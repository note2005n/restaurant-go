FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -o /app .

FROM alpine:3.20
WORKDIR /data
COPY --from=build /app /app
ENV PORT=8080 DB_PATH=/data/restaurant.db COOKIE_SECURE=1
EXPOSE 8080
CMD ["/app"]
