#!/usr/bin/env bash
#
# End-to-end test against a throwaway Omni and real Talos machines.
#
# Brings up Omni (docker compose), boots QEMU VMs from an Image Factory ISO carrying the SideroLink
# kernel args so they join Omni in maintenance mode, then runs the integration test binary, which
# drives the siderolabs-mcp-server binary over stdio. Everything is torn down on exit; logs are kept
# in _out/e2e.
#
# Requires root (the QEMU provisioner manages bridges/taps), KVM, docker, crane, curl, jq, envsubst,
# openssl and gpg. Run via `make e2e-qemu` (which builds the binaries first), as root:
#
#   sudo -E make e2e-qemu
#
# Environment:
#   OMNI_VERSION        Omni image tag (default: latest).
#   TALOS_VERSION       Talos version for boot media and the cluster (default: latest stable).
#   MACHINE_COUNT       Number of VMs (default: 1).
#   QEMU_MEMORY         Per-VM memory in MiB (default: 3072).
#   QEMU_CPUS           Per-VM vCPUs (default: 2).
#   FACTORY_URL         Image Factory URL (default: https://factory.talos.dev).
#   E2E_TEST_ARGS       Extra args for the test binary (e.g. "-test.run TestE2E/factory").

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

export OMNI_VERSION="${OMNI_VERSION:-latest}"
export OMNI_HOST_PORT="${OMNI_HOST_PORT:-8099}"
export OMNI_MACHINE_API_HOST_PORT="${OMNI_MACHINE_API_HOST_PORT:-8090}"
export OMNI_SIDEROLINK_HOST_PORT="${OMNI_SIDEROLINK_HOST_PORT:-50180}"

MACHINE_COUNT="${MACHINE_COUNT:-1}"
QEMU_MEMORY="${QEMU_MEMORY:-3072}"
QEMU_CPUS="${QEMU_CPUS:-2}"
FACTORY_URL="${FACTORY_URL:-https://factory.talos.dev}"

QEMU_CLUSTER=mcp-e2e
QEMU_CIDR=10.5.0.0/24
# talosctl assigns the first address of the CIDR to the host bridge, VMs reach Omni through it
export SIDEROLINK_HOST=10.5.0.1

ARCH=amd64
SERVER_BINARY="${REPO_ROOT}/_out/siderolabs-mcp-server-linux-${ARCH}"
TEST_BINARY="${REPO_ROOT}/_out/integration-test-linux-${ARCH}"

export E2E_DIR="${REPO_ROOT}/_out/e2e"

# CI runners don't ship the compose plugin
if [[ "${CI:-false}" == "true" ]] && ! docker compose version >/dev/null 2>&1; then
  COMPOSE_VERSION=v2.32.4
  install -d /usr/local/lib/docker/cli-plugins
  curl -fsSL "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-$(uname -m)" \
    -o /usr/local/lib/docker/cli-plugins/docker-compose
  chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
fi

COMPOSE=(docker compose -p mcp-e2e -f hack/test/docker-compose.yaml)

for bin in docker crane curl jq envsubst openssl gpg; do
  command -v "${bin}" >/dev/null || { echo "required binary not found: ${bin}" >&2; exit 1; }
done

[[ -e /dev/kvm ]] || { echo "/dev/kvm is not available" >&2; exit 1; }
[[ -x "${SERVER_BINARY}" && -x "${TEST_BINARY}" ]] || { echo "build first: make e2e-qemu" >&2; exit 1; }

if [[ -z "${TALOS_VERSION:-}" ]]; then
  TALOS_VERSION="$(curl -fsSL "${FACTORY_URL}/versions" | jq -r '[.[] | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))] | last')"
fi
TALOS_VERSION="v${TALOS_VERSION#v}"

rm -rf "${E2E_DIR}"
mkdir -p "${E2E_DIR}/certs" "${E2E_DIR}/omni" "${E2E_DIR}/bin"

# kept outside the repo: QEMU socket paths inside it must stay under 108 bytes
TALOS_STATE="$(mktemp -d /tmp/mcp-e2e.XXXXXX)"
TALOSCTL="${E2E_DIR}/bin/talosctl"
OMNICTL="${E2E_DIR}/bin/omnictl"

cleanup() {
  echo "==> Tearing down"
  mkdir -p "${E2E_DIR}/logs"
  cp "${TALOS_STATE}"/*/*.log "${E2E_DIR}/logs/" 2>/dev/null || true
  "${TALOSCTL}" cluster destroy --provisioner=qemu --name="${QEMU_CLUSTER}" --state="${TALOS_STATE}" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" logs --no-color >"${E2E_DIR}/omni.log" 2>&1 || true
  "${COMPOSE[@]}" down -t 5 -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "${E2E_DIR}/certs" "${E2E_DIR}/omni" "${E2E_DIR}/bin" "${TALOS_STATE}"
}
trap cleanup EXIT INT TERM

echo "==> Generating throwaway TLS certificates"
(
  cd "${E2E_DIR}/certs"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
    -subj "/CN=mcp-e2e CA" -keyout ca-key.pem -out ca.pem 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=localhost" -keyout omni-key.pem -out omni.csr 2>/dev/null
  openssl x509 -req -in omni.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -days 1 \
    -extfile <(printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth') \
    -out omni.pem 2>/dev/null
)
# trusted by omnictl, the MCP server and the test binary instead of skipping TLS verification
export SSL_CERT_FILE="${E2E_DIR}/certs/ca.pem"

echo "==> Generating throwaway etcd encryption PGP key"
GNUPGHOME="$(mktemp -d)"
export GNUPGHOME
gpg --batch --passphrase '' --quick-generate-key "Omni E2E <e2e@siderolabs.com>" rsa4096 cert never 2>/dev/null
FPR="$(gpg --list-keys --with-colons | awk -F: '/^fpr/ { print $10; exit }')"
gpg --batch --passphrase '' --quick-add-key "${FPR}" rsa4096 encr never 2>/dev/null
gpg --armor --export-secret-keys "${FPR}" >"${E2E_DIR}/certs/key.private"
gpg --armor --export "${FPR}" >"${E2E_DIR}/certs/key.public"
rm -rf "${GNUPGHOME}"
unset GNUPGHOME

# the Omni container runs as non-root
chmod -R a+rX "${E2E_DIR}/certs"
chmod a+rwx "${E2E_DIR}/omni"

echo "==> Fetching talosctl ${TALOS_VERSION} and omnictl ${OMNI_VERSION}"
crane export "ghcr.io/siderolabs/talosctl:${TALOS_VERSION}" - | tar x -C "${E2E_DIR}/bin" talosctl
crane export "ghcr.io/siderolabs/omni:${OMNI_VERSION}" - |
  tar x -C "${E2E_DIR}/bin" --strip-components=1 "omnictl/omnictl-linux-${ARCH}"
mv "${E2E_DIR}/bin/omnictl-linux-${ARCH}" "${OMNICTL}"
chmod +x "${TALOSCTL}" "${OMNICTL}"

echo "==> Starting Omni ${OMNI_VERSION}"
envsubst <hack/test/omni-config.yaml.tmpl >"${E2E_DIR}/omni-config.yaml"
"${COMPOSE[@]}" down -t 5 -v --remove-orphans >/dev/null 2>&1 || true
"${COMPOSE[@]}" up -d --wait dex
"${COMPOSE[@]}" up -d omni

deadline=$((SECONDS + 180))
until [[ -s "${E2E_DIR}/omni/key" ]]; do
  if ((SECONDS > deadline)); then
    echo "Omni did not write the service account key in time" >&2
    "${COMPOSE[@]}" logs --no-color omni | tail -50 >&2
    exit 1
  fi
  sleep 2
done

export OMNI_ENDPOINT="https://127.0.0.1:${OMNI_HOST_PORT}"
OMNI_SERVICE_ACCOUNT_KEY="$(cat "${E2E_DIR}/omni/key")"
export OMNI_SERVICE_ACCOUNT_KEY

deadline=$((SECONDS + 120))
until JOIN_TOKEN="$("${OMNICTL}" get defaultjointoken -ojson 2>/dev/null | jq -er '.spec.tokenid')"; do
  ((SECONDS < deadline)) || { echo "failed to read the join token" >&2; exit 1; }
  sleep 2
done

echo "==> Creating boot media with SideroLink kernel args"
SCHEMATIC_ID="$(
  jq -n --arg api "siderolink.api=grpc://${SIDEROLINK_HOST}:${OMNI_MACHINE_API_HOST_PORT}?jointoken=${JOIN_TOKEN}" '{
    customization: {
      extraKernelArgs: [
        $api,
        "talos.events.sink=[fdae:41e4:649b:9303::1]:8091",
        "talos.logging.kernel=tcp://[fdae:41e4:649b:9303::1]:8092",
        "console=ttyS0"
      ]
    }
  }' | curl -fsSL -X POST --data-binary @- "${FACTORY_URL}/schematics" | jq -er '.id'
)"

echo "==> Booting ${MACHINE_COUNT} QEMU machine(s) in maintenance mode"
# --skip-injecting-config writes the unused machine configs to the working directory
cd "${TALOS_STATE}"
"${TALOSCTL}" --talosconfig="${TALOS_STATE}/talosconfig" cluster create dev \
  --provisioner=qemu \
  --name="${QEMU_CLUSTER}" \
  --state="${TALOS_STATE}" \
  --controlplanes="${MACHINE_COUNT}" \
  --workers=0 \
  --cidr="${QEMU_CIDR}" \
  --mtu=1430 \
  --memory="${QEMU_MEMORY}" \
  --cpus="${QEMU_CPUS}" \
  --with-uuid-hostnames \
  --skip-injecting-config \
  --wait=false \
  --iso-path="${FACTORY_URL}/image/${SCHEMATIC_ID}/${TALOS_VERSION}/metal-${ARCH}.iso"

cd "${REPO_ROOT}"

echo "==> Running e2e tests"
E2E_SERVER_BINARY="${SERVER_BINARY}" \
  E2E_TALOS_VERSION="${TALOS_VERSION}" \
  E2E_MACHINE_COUNT="${MACHINE_COUNT}" \
  "${TEST_BINARY}" -test.v -test.timeout 60m ${E2E_TEST_ARGS:-}
