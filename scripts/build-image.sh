#!/bin/sh
# Build a Warden container image: scripts/build-image.sh IMAGE[:TAG]
#
# IMAGE is required; there is no default registry. The image holds the warden
# binary (built here, CGO_ENABLED=0), the packs, and kubectl (downloaded from
# dl.k8s.io and checked against its published sha256).
#
#   ARCH=amd64|arm64     target architecture (default amd64)
#   KUBECTL=v1.30.14     kubectl version; KUBECTL=none leaves it out
#   EXTRA_BINS="a b"     more binaries to copy into /usr/local/bin (verify them yourself)
#   APK_PACKAGES="git=2.45.4-r0"  pinned Alpine packages to add (the github-repo pack needs git)
#   VERSION=v0.1.0       the version baked into the binary (default: git describe)
#   PUSH=1               docker push the image and print its digest
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
IMAGE=${1:?usage: build-image.sh IMAGE[:TAG]  (no default registry)}
ARCH=${ARCH:-amd64}
KUBECTL=${KUBECTL:-v1.30.14}
CTX=$(mktemp -d "${TMPDIR:-/tmp}/warden-image.XXXXXX")
mkdir -p "$CTX/bin"
(cd "$HERE" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/nenkoru/warden/internal/version.Version=${VERSION:-$(git -C "$HERE" describe --tags --always 2>/dev/null || echo dev)}" -o "$CTX/bin/warden" ./cmd/warden)
if [ "$KUBECTL" != none ]; then
	curl -fsSLo "$CTX/bin/kubectl" "https://dl.k8s.io/release/$KUBECTL/bin/linux/$ARCH/kubectl"
	echo "$(curl -fsSL "https://dl.k8s.io/release/$KUBECTL/bin/linux/$ARCH/kubectl.sha256")  $CTX/bin/kubectl" | shasum -a 256 -c -
fi
for b in ${EXTRA_BINS:-}; do
	cp "$b" "$CTX/bin/"
done
chmod +x "$CTX"/bin/*
cp -R "$HERE/packs" "$CTX/packs"
docker build --platform "linux/$ARCH" --build-arg APK_PACKAGES="${APK_PACKAGES:-}" -f "$HERE/deploy/Dockerfile" -t "$IMAGE" "$CTX"
if [ "${PUSH:-0}" = 1 ]; then
	docker push "$IMAGE"
	docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE"
fi
echo "build context: $CTX"
