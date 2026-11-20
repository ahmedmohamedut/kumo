#!/usr/bin/env bash
# render-cilium.sh -- regenerate files/cilium.yaml from the pinned Cilium chart.
#
# Run by a person bumping Cilium, never by the build: the rendered manifest is
# committed, so a Cilium upgrade is a reviewable diff and the image build needs
# no Helm (docs/design/08-kubernetes.md section 7). Needs helm and curl.
#
#   ./render-cilium.sh          # after changing CILIUM_VERSION / CHART_SHA256
#
# The chart archive is checked against CHART_SHA256, the digest helm.cilium.io's
# index publishes for that version; a mismatch stops before anything is
# rendered. The values are cilium-values.yaml beside this script. The render is
# deterministic (no generated certificates with these values), so running it
# twice with nothing changed leaves the tree clean.
#
# It also rewrites the Cilium lines of files/images.txt from the images the
# manifest references, each pinned by digest.
set -euo pipefail
export LC_ALL=C

CILIUM_VERSION=1.20.5
CHART_SHA256=0c7592eb75179ecf46dd3ec22145916f0164c4401214ea15afb5b230b1d11a21

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/cilium.tgz" "https://helm.cilium.io/cilium-$CILIUM_VERSION.tgz"
echo "$CHART_SHA256  $work/cilium.tgz" | sha256sum -c --quiet -

# Helm's own repositories and caches are kept out of it: a stale entry in the
# caller's helm config would otherwise fail the render.
export HELM_CONFIG_HOME=$work/config HELM_CACHE_HOME=$work/cache HELM_DATA_HOME=$work/data

{
	printf '# Cilium %s, rendered by ../render-cilium.sh from the chart with sha256\n' "$CILIUM_VERSION"
	printf '# %s and ../cilium-values.yaml. Do not edit by hand.\n' "$CHART_SHA256"
	helm template cilium "$work/cilium.tgz" --namespace kube-system -f "$here/cilium-values.yaml"
} >"$work/cilium.yaml"

if grep -qE 'BEGIN (CERTIFICATE|RSA|EC|PRIVATE)' "$work/cilium.yaml"; then
	echo "render-cilium.sh: the render embeds key material; these values must not generate certificates" >&2
	exit 1
fi
mv "$work/cilium.yaml" "$here/files/cilium.yaml"

# Every image the manifest names, which the build pre-pulls. Each must carry a
# digest: a tag alone could resolve to a different image on the next build.
images=$(sed -n 's/^ *image: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$here/files/cilium.yaml" | sort -u)
for ref in $images; do
	case $ref in
	*@sha256:*) ;;
	*) echo "render-cilium.sh: $ref is not pinned by digest" >&2; exit 1 ;;
	esac
done
{
	grep -v '^quay.io/cilium/' "$here/files/images.txt" 2>/dev/null || true
	printf '%s\n' $images
} | sort -u >"$work/images.txt"
mv "$work/images.txt" "$here/files/images.txt"

echo "rendered files/cilium.yaml for Cilium $CILIUM_VERSION ($(wc -l <"$here/files/cilium.yaml") lines)"
