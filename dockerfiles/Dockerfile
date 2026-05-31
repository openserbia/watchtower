# distroless static-debian13 already bundles ca-certificates
# (/etc/ssl/certs/ca-certificates.crt) and tzdata (/usr/share/zoneinfo), so the
# previous Alpine cert/tzdata stage is gone. The default tag runs as root
# (uid 0), which Watchtower needs for Docker socket access — do NOT switch to
# :nonroot.
FROM gcr.io/distroless/static-debian13:latest@sha256:3592aa8171c77482f62bbc4164e6a2d141c6122554ace66e5cc910cadb961ff0
LABEL "com.centurylinklabs.watchtower"="true"

EXPOSE 8080

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/watchtower /

HEALTHCHECK CMD [ "/watchtower", "--health-check"]

ENTRYPOINT ["/watchtower"]
