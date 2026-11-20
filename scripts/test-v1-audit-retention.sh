#!/usr/bin/env bash
set -euo pipefail
umask 077

evidence=${REGATE_AUDIT_EVIDENCE_DIR:?set REGATE_AUDIT_EVIDENCE_DIR to the signed evidence directory from make test-v1-controlled}
report_dir="$PWD/.state/v1-controlled"
scratch=$(mktemp -d)
container=
cleanup() {
    if [[ -n "$container" ]]; then
        docker rm -fv "$container" >/dev/null || true
    fi
    rm -r "$scratch"
}
trap cleanup EXIT
mkdir -p "$report_dir" "$scratch/restore"
exec > >(tee "$report_dir/audit-retention.log") 2>&1

verify_signature() {
    openssl pkeyutl -verify -pubin -inkey "$evidence/public.pem" -rawin \
        -in "$1/checkpoint.json" -sigfile "$1/checkpoint.sig"
}

wait_for_storage() {
    for ((attempt=0; attempt<45; attempt++)); do
        if s3 --fail --silent --max-time 2 -o /dev/null "$1" 2>/dev/null; then
            return
        fi
        sleep 1
    done
    printf '%s\n' 'Audit storage did not become ready'
    return 1
}

storage_url() {
    local authority
    authority=$(docker port "$container" 9000/tcp) || return
    [[ "$authority" =~ ^127\.0\.0\.1:[0-9]+$ ]] || return 1
    printf 'http://%s\n' "$authority"
}

s3() {
    curl --config "$scratch/client" "$@"
}

put_version() {
    local object=$1 file=$2
    shift 2
    local checksum
    checksum=$(openssl dgst -sha256 -binary "$file" | openssl base64 -A) || return
    s3 --fail -X PUT -D "$scratch/headers" -o /dev/null \
        -H "x-amz-checksum-sha256: $checksum" --data-binary "@$file" "$@" "$object" || return
    awk 'tolower($1)=="x-amz-version-id:" { sub(/\r$/, "", $2); print $2 }' "$scratch/headers"
}

main() {
    local image access secret base object retain_until locked unlocked status
    image='docker.io/rustfs/rustfs@sha256:b81d16a90ee25bd9511280127ca1471788c6e874cf0f0da759645f46b2dc1d74'
    printf 'audit_storage_image=%s\n' "$image"
    verify_signature "$evidence"
    tar -cf "$scratch/checkpoint.tar" -C "$evidence" checkpoint.json checkpoint.sig final.json
    access=$(openssl rand -hex 8)
    secret=$(openssl rand -hex 32)
    printf 'RUSTFS_ACCESS_KEY=%s\nRUSTFS_SECRET_KEY=%s\nRUSTFS_CONSOLE_ENABLE=false\n' "$access" "$secret" >"$scratch/storage.env"
    printf 'user = "%s:%s"\naws-sigv4 = "aws:amz:us-east-1:s3"\nnoproxy = "*"\nsilent\nshow-error\nmax-time = 10\n' "$access" "$secret" >"$scratch/client"
    container=$(docker run -d --rm --memory 512m --publish 127.0.0.1::9000 --volume /data --env-file "$scratch/storage.env" "$image")
    base=$(storage_url)
    wait_for_storage "$base/"
    object="$base/regate-audit/checkpoint.tar"
    s3 --fail -X PUT -H 'x-amz-bucket-object-lock-enabled: true' -o /dev/null "$base/regate-audit"
    retain_until=$(date -u -d '1 day' '+%Y-%m-%dT%H:%M:%SZ')
    locked=$(put_version "$object" "$scratch/checkpoint.tar" -H 'x-amz-object-lock-mode: COMPLIANCE' \
        -H "x-amz-object-lock-retain-until-date: $retain_until")
    printf '%s\n' 'unretained control version' >"$scratch/control"
    unlocked=$(put_version "$object" "$scratch/control")
    [[ "$locked" =~ ^[[:alnum:]-]+$ && "$unlocked" =~ ^[[:alnum:]-]+$ && "$locked" != "$unlocked" ]]
    printf 'locked_version=%s retain_until=%s\n' "$locked" "$retain_until"
    docker restart "$container" >/dev/null
    base=$(storage_url)
    object="$base/regate-audit/checkpoint.tar"
    wait_for_storage "$object?versionId=$locked"

    status=$(s3 -X DELETE -o /dev/null -w '%{http_code}' "$object?versionId=$unlocked")
    [[ "$status" == 204 ]] || { printf 'Expected deletion of control version, got HTTP %s\n' "$status"; return 1; }
    status=$(s3 -X DELETE -o /dev/null -w '%{http_code}' "$object?versionId=$locked")
    [[ "$status" == 403 ]] || { printf 'Expected denial for locked version, got HTTP %s\n' "$status"; return 1; }
    s3 --fail -o "$scratch/retention.xml" "$object?retention&versionId=$locked"
    grep -Fq '<Mode>COMPLIANCE</Mode>' "$scratch/retention.xml"
    s3 --fail -o "$scratch/restored.tar" "$object?versionId=$locked"
    cmp "$scratch/checkpoint.tar" "$scratch/restored.tar"
    tar -xf "$scratch/restored.tar" -C "$scratch/restore"
    verify_signature "$scratch/restore"
    printf 'object_lock=COMPLIANCE control_delete_http=204 locked_delete_http=403 restored_signature=verified\n'
}

main
