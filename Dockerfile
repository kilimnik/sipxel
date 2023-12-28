FROM registry.hub.docker.com/library/golang:1.20 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN go vet -v
RUN go test -v

RUN CGO_ENABLED=0 go build -o /go/bin/app

FROM registry.hub.docker.com/library/alpine

COPY --from=build /go/bin/app /

ARG PASSWORD
ARG USERNAME
ARG SERVER
ARG PROTOCOL

CMD ["sh", "-c", "/app -p $PASSWORD -u $USERNAME -srv $SERVER -t $PROTOCOL"]