FROM golang:1.11-alpine

RUN ["apk", "add", "git"]
WORKDIR /ws
COPY go.mod *.go ./
ENV CGO_ENABLED=0
RUN ["go", "build", "-o", "main", "-a", "-ldflags", "-extldflags \"-static\"", "."]

FROM scratch

COPY --from=0 /ws/main /main

ENTRYPOINT ["/main"]
