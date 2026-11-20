#!/usr/bin/env bash
set -euo pipefail
umask 077

report_dir="$PWD/.state/v1-controlled"
mkdir -p "$report_dir"
scratch=$(mktemp -d)
container=
cleanup() {
    if [[ -n "$container" ]]; then
        docker rm -f "$container" >/dev/null || true
    fi
    rm -r "$scratch"
}
trap cleanup EXIT

export REGATE_CONTROLLED_NGINX="${REGATE_CONTROLLED_NGINX:-nginx}"
export REGATE_E2E_CHROMIUM_EXECUTABLE="${REGATE_E2E_CHROMIUM_EXECUTABLE:-$(command -v chromium || true)}"
export REGATE_CONTROLLED_CLIENT_SECRET
REGATE_CONTROLLED_CLIENT_SECRET=$(openssl rand -hex 32)
export REGATE_CONTROLLED_TLS_CERT="$scratch/ingress.pem"
export REGATE_CONTROLLED_TLS_KEY="$scratch/ingress.key"
mkdir "$scratch/import"

make_realm() {
    local realm=$1
    jq -n --arg realm "$realm" '
      ["acceptance:access", "summary:write", "summary:read", "extra:read"] as $scopes |
      {
        realm: $realm,
        enabled: true,
        sslRequired: "none",
        accessTokenLifespan: 1200,
        clientScopes: ($scopes | map({
          name: ., protocol: "openid-connect",
          attributes: {"include.in.token.scope": "true"}
        })),
        clients: (["test-user", "another-test-user", "soak-reader"] +
          [range(8) | "soak-principal-\(.)"] | map({
            clientId: ., enabled: true, protocol: "openid-connect",
            clientAuthenticatorType: "client-secret", publicClient: false,
            secret: env.REGATE_CONTROLLED_CLIENT_SECRET,
            serviceAccountsEnabled: true, standardFlowEnabled: false,
            directAccessGrantsEnabled: false, optionalClientScopes: $scopes,
            protocolMappers: [{
              name: "Regate audience", protocol: "openid-connect",
              protocolMapper: "oidc-audience-mapper",
              config: {"included.custom.audience": "regate-local-acceptance",
                       "access.token.claim": "true"}
            }]
          }))
      }' >"$scratch/import/$realm-realm.json"
    chmod 0444 "$scratch/import/$realm-realm.json"
}

run() {
    local image imageID authority endpoint ready
    image='quay.io/keycloak/keycloak@sha256:7e66a96e4739239070aeaf72e4c5decf0f7f682cfc725b9f7ea2b336eaba38b3'
    imageID=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null) || {
        docker pull "$image"
        imageID=$(docker image inspect --format '{{.Id}}' "$image")
    }
    printf 'keycloak_image=%s\n' "$imageID"
    "$REGATE_CONTROLLED_NGINX" -v 2>&1
    make_realm regate-primary
    make_realm regate-secondary
    chmod 0755 "$scratch/import"
    container=$(docker run -d --rm --memory 1g --publish 127.0.0.1::8080 \
        --env 'JAVA_OPTS_KC_HEAP=-Xms128m -Xmx384m' \
        --mount "type=bind,src=$scratch/import,dst=/opt/keycloak/data/import,readonly" \
        "$image" start-dev --import-realm)
    authority=$(docker port "$container" 8080/tcp)
    [[ "$authority" =~ ^127\.0\.0\.1:[0-9]+$ ]]
    export REGATE_CONTROLLED_IDP_URL="http://$authority"
    ready=
    endpoint="$REGATE_CONTROLLED_IDP_URL/realms/regate-primary/.well-known/openid-configuration"
    for ((attempt=0; attempt<90; attempt++)); do
        if curl --fail --silent --max-time 2 "$endpoint" >/dev/null; then
            ready=1
            break
        fi
        sleep 1
    done
    [[ "$ready" == 1 ]] || { printf '%s\n' 'Keycloak did not become ready'; return 1; }
    openssl req -x509 -newkey rsa:2048 -days 1 -nodes -subj /CN=127.0.0.1 \
        -addext subjectAltName=IP:127.0.0.1 -keyout "$REGATE_CONTROLLED_TLS_KEY" -out "$REGATE_CONTROLLED_TLS_CERT" 2>/dev/null
    export SSL_CERT_FILE="$REGATE_CONTROLLED_TLS_CERT"
    export NODE_EXTRA_CA_CERTS="$REGATE_CONTROLLED_TLS_CERT"
    bash scripts/test-v1-full.sh
    bash scripts/test-v1-soak.sh
}

printf 'Controlled acceptance report: %s/latest.log\n' "$report_dir"
exec > >(tee "$report_dir/latest.log") 2>&1
run
