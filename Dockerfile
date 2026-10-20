FROM golang:1.23.6-alpine AS build
COPY docker/certs/ /usr/local/share/ca-certificates/
RUN update-ca-certificates && apk add --no-cache make
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY Makefile ./
COPY api/ ./api/
RUN make generate
COPY . .
RUN CGO_ENABLED=0 make build

FROM build AS test
RUN apk add --no-cache gcc musl-dev
CMD ["go", "test", "-race", "-count=1", "-v", "./..."]

FROM scratch AS app
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /src/bin/marketplace /app/marketplace
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/marketplace"]
