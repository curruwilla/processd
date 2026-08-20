# Built by GoReleaser from the binaries it already cross-compiled; see
# .goreleaser.yml. Nothing is compiled here.
#
# Alpine, not scratch: processd supervises CLI processes, so the image has to
# carry a usable userland for the commands the workers declare.
FROM alpine:3.24

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata \
	&& mkdir -p /etc/processd/workers.d /var/lib/processd /var/log/processd

COPY $TARGETPLATFORM/processd /usr/bin/processd
COPY packaging/docker/entrypoint.sh /usr/local/bin/processd-entrypoint

EXPOSE 7373

ENTRYPOINT ["/usr/local/bin/processd-entrypoint"]
CMD ["serve"]
