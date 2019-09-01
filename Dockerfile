FROM monstar/docker-updater:base

ENV SRCPATH "$GOPATH/src/github.com/monstarnn/docker-updater"

COPY ./main.go "$SRCPATH/main.go"

RUN cd $SRCPATH && go install -v

FROM ubuntu:18.10

COPY --from=0 /go/bin/docker-updater /usr/bin/docker-updater

CMD [ "docker-updater" ]

EXPOSE 8084
