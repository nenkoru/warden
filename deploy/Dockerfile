# Warden in a container: the static binary, the packs, and whatever tools the
# packs need (scripts/build-image.sh puts kubectl in, plus any extra binaries
# you hand it). No RUN step: the build context brings verified binaries.
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
COPY bin/ /usr/local/bin/
COPY packs /opt/warden/packs
ENV WARDEN_PACKS=/opt/warden/packs HOME=/state
USER 65532:65532
WORKDIR /state
ENTRYPOINT ["warden"]
CMD ["listen", "--config", "/config/warden.toml"]
