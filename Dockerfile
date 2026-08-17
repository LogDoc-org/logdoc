# --- UI ---
FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json* ./
RUN npm install
COPY ui/ .
RUN npm run build

# --- Go ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /logdoc ./cmd/logdoc

# --- Runtime ---
FROM alpine:3.21
RUN adduser -D -H logdoc
USER logdoc
COPY --from=build /logdoc /usr/local/bin/logdoc
EXPOSE 9001 9999/tcp 9999/udp
ENTRYPOINT ["logdoc"]
