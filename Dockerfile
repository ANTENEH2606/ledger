# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /ledger .

# ---- run stage: small image with only the binary ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /ledger /ledger
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/ledger"]