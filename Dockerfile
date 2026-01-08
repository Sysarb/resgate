FROM golang AS builder

LABEL maintainer="Sysarb <devops@sysarb.se>"
ENV GO111MODULE=on

WORKDIR /src/resgate

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -v -ldflags "-s -w" -o /resgate

FROM scratch
COPY --from=builder /resgate /resgate
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

ENTRYPOINT ["/resgate"]
CMD [""]
