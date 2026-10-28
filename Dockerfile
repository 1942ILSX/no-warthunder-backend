FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /battlecity . \
    && mkdir -p /data && chown 10001:10001 /data

FROM scratch
COPY --from=build /battlecity /battlecity
COPY --from=build --chown=10001:10001 /data /data
COPY --from=build /src/models /models
USER 10001:10001
ENV ADDR=0.0.0.0:8080 MAX_ROOMS=25 LEADERBOARD_FILE=/data/leaderboard.json MODELS_DIR=/models GOMEMLIMIT=160MiB GOMAXPROCS=1
EXPOSE 8080
ENTRYPOINT ["/battlecity"]
