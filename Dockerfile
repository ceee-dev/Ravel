FROM golang:1.17-alpine
RUN echo '@edgemain http://dl-3.alpinelinux.org/alpine/edge/main' >> /etc/apk/repositories
RUN apk add libpcap iptables haproxy iproute2 ipvsadm@edgemain gcc libc-dev git libpcap-dev && rm -rf /var/cache/apk/*
WORKDIR /app/src
COPY . /app/src
WORKDIR /app/src/cmd/ravel
RUN CGO_ENABLED=1 go build -v -o /app/src/cmd/ravel/ravel
ADD https://github.com/osrg/gobgp/releases/download/v2.22.0/gobgp_2.22.0_linux_amd64.tar.gz gobgp_2.22.0_linux_amd64.tar.gz
RUN tar zxf gobgp_2.22.0_linux_amd64.tar.gz 
RUN ls -al


FROM golang:1.17-alpine
ARG SKIP_MASTER_NODE=N
ARG RAVEL_LOGRULE=N
ENV SKIP_MASTER_NODE=$SKIP_MASTER_NODE
ENV RAVEL_LOGRULE=$RAVEL_LOGRULE
LABEL MAINTAINER='RDEI Team <rdei@comcast.com>'
RUN echo '@edgemain http://dl-3.alpinelinux.org/alpine/edge/main' >> /etc/apk/repositories
RUN apk add libpcap iptables haproxy iproute2 ipvsadm@edgemain gcc libc-dev git libpcap-dev && rm -rf /var/cache/apk/*; rm -rf /var/cache/apk/*
COPY --from=0 /app/src/cmd/ravel/ravel /bin/
COPY --from=0 /app/src/cmd/ravel/ravel /bin/kube2ipvs
COPY --from=0 /app/src/cmd/ravel/gobgp /bin/
COPY --from=0 /app/src/cmd/ravel/gobgpd /bin/
RUN chmod 750 /bin/gobgp /bin/gobgpd /bin/ravel
ENTRYPOINT ["/bin/ravel"]
