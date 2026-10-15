FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -o /app .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /app /app
COPY --from=build /src/static /app/static
ENV PORT=8080 DB_PATH=/data/restaurant.db COOKIE_SECURE=1
EXPOSE 8080
CMD ["/app"]