
FROM golang:1.16
WORKDIR /app
COPY . .
ENV GOSUMDB=off
RUN go mod tidy
WORKDIR /app/cmd
RUN CGO_ENABLED=0 go build -o gocsp main.go

FROM alpine:3.14
COPY --from=0 /app/cmd/gocsp /
CMD ["/gocsp"]