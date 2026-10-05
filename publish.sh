#!/bin/sh
# Publish server.json to the MCP registry under the disputes.online namespace.
#
# The registry accepts the publication because disputes.online serves the public
# half of this key at /.well-known/mcp-registry-auth. The private half never
# belongs in the repository.
set -eu

KEY="${MCP_REGISTRY_KEY:-$HOME/.mcp-registry/disputes.online.pem}"
if [ ! -f "$KEY" ]; then
    echo "private key not found: $KEY" >&2
    exit 1
fi

PRIVATE_KEY="$(openssl pkey -in "$KEY" -noout -text | grep -A3 "priv:" | tail -n +2 | tr -d ' :\n')"

cd "$(dirname "$0")"
mcp-publisher login http --domain disputes.online --private-key "$PRIVATE_KEY"
mcp-publisher publish
