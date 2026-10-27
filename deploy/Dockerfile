# Warden in a container: the static binary, the packs, and whatever tools the
# packs need (scripts/build-image.sh puts kubectl in, plus any extra binaries
# you hand it). Optional Alpine packages (pinned, e.g. git for the
# github-repo pack) come from APK_PACKAGES; empty = no RUN effect.
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
ARG APK_PACKAGES=""
RUN if [ -n "$APK_PACKAGES" ]; then apk add --no-cache $APK_PACKAGES; fi
COPY bin/ /usr/local/bin/
COPY packs /opt/warden/packs
ENV WARDEN_PACKS=/opt/warden/packs HOME=/state
USER 65532:65532
WORKDIR /state
ENTRYPOINT ["warden"]
CMD ["listen", "--config", "/config/warden.toml"]
