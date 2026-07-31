# Thin runtime image: static kscale binaries (staged by build.sh) on alpine.
# iproute2 is for in-container network setup/debug (ip link, etc.).
FROM alpine:latest
RUN apk add --no-cache iproute2 curl tcpdump ethtool
COPY stage/bin/ /usr/local/bin/
COPY stage/objs/ /objs/
