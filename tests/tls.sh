# Sourced by the test scripts.

# Creates a throwaway certificate for the coordinator.
#   make_tls_cert DIR   -> writes DIR/tls.crt and DIR/tls.key
make_tls_cert() {
    local dir="$1"
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
        -keyout "$dir/tls.key" -out "$dir/tls.crt" \
        -subj "/CN=minibox-test" \
        -addext "subjectAltName=IP:127.0.0.1,IP:10.99.0.1,DNS:localhost" \
        -addext "basicConstraints=critical,CA:TRUE" >/dev/null 2>&1
}

# Prints the SHA-256 of a token, the form token files store.
token_hash() {
    printf '%s' "$1" | sha256sum | cut -d' ' -f1
}
