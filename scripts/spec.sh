#!/bin/sh
# Fetch or re-pin the Immich OpenAPI spec used to generate the API client.
#
#   scripts/spec.sh fetch            download the pinned spec and verify it
#   scripts/spec.sh update v3.3.0    pin a new release and download its spec
set -eu

cd "$(dirname "$0")/.."
PIN=api/spec.mk
SPEC=api/immich-openapi-specs.json
REPO=https://github.com/immich-app/immich

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
	else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

pin() { sed -n "s/^$1 := //p" "$PIN"; }

download() { # commit dest
	curl -sfL --retry 3 -o "$2" \
		"https://raw.githubusercontent.com/immich-app/immich/$1/open-api/immich-openapi-specs.json"
}

case "${1:-}" in
fetch)
	commit=$(pin IMMICH_COMMIT)
	want=$(pin SPEC_SHA256)
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	download "$commit" "$tmp"
	got=$(sha256 "$tmp")
	if [ "$got" != "$want" ]; then
		echo "checksum mismatch for $(pin IMMICH_VERSION) ($commit):" >&2
		echo "  want $want" >&2
		echo "  got  $got" >&2
		exit 1
	fi
	mv "$tmp" "$SPEC"
	trap - EXIT
	echo "$SPEC: $(pin IMMICH_VERSION) @ $commit, sha256 verified"
	;;
update)
	version=${2:?usage: scripts/spec.sh update vX.Y.Z}
	# Annotated tags list the tagged commit as ^{}; lightweight ones do not.
	refs=$(git ls-remote "$REPO" "refs/tags/$version" "refs/tags/$version^{}")
	commit=$(echo "$refs" | awk '/\^\{\}$/ {print $1}')
	[ -n "$commit" ] || commit=$(echo "$refs" | awk 'NR==1 {print $1}')
	[ -n "$commit" ] || { echo "tag $version not found in $REPO" >&2; exit 1; }
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	download "$commit" "$tmp"
	sum=$(sha256 "$tmp")
	mv "$tmp" "$SPEC"
	trap - EXIT
	cat >"$PIN" <<PIN
# Pinned Immich OpenAPI spec. Written by \`make spec-update\`; do not edit by hand.
IMMICH_VERSION := $version
IMMICH_COMMIT := $commit
SPEC_SHA256 := $sum
PIN
	bare=${version#v}
	sed -i.bak "s/^const SpecVersion = \".*\"/const SpecVersion = \"$bare\"/" internal/immich/client.go
	sed -i.bak "s#ghcr.io/immich-app/immich-server:v[^ ]*#ghcr.io/immich-app/immich-server:$version#" test/integration/docker-compose.yml
	rm -f internal/immich/client.go.bak test/integration/docker-compose.yml.bak
	echo "pinned $version @ $commit (sha256 $sum)"
	echo "next: make generate && make test, then make immich-down immich-up integration"
	echo "note: the integration compose file also pins postgres/valkey images; compare with"
	echo "      $REPO/releases/download/$version/docker-compose.yml"
	;;
*)
	echo "usage: $0 fetch | update vX.Y.Z" >&2
	exit 2
	;;
esac
