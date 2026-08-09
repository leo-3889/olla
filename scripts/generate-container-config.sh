#!/bin/bash
# creates a Container configuration file from the base configuration.

# Update endpoints to use host.docker.internal for local development
sed 's|- url: "http://localhost:|- url: "http://host.docker.internal:|g; s|- url: "http://127\.0\.0\.1:|- url: "http://host.docker.internal:|g' config/config.yaml > config/docker.yaml

# update the lmstudio port from 11234 to 1234 (default)
sed -i 's|http://host.docker.internal:11234|http://host.docker.internal:1234|g' config/docker.yaml

# update the host: "localhost" to be 0.0.0.0 for Docker compatibility
sed -i 's|host: "localhost"|host: "0.0.0.0"|g' config/docker.yaml

# update the header  from '# Olla Configuration (default)' to '# Olla Configuration (docker)'
sed -i 's|# Olla Configuration (default)|# Olla Configuration (docker)|g' config/docker.yaml

# Widen the dashboard CIDRs to the private ranges: traffic through a published
# port arrives from the bridge gateway (172.17.0.1), so loopback-only would 403.
sed -i 's|^      - "::1/128"$|      - "::1/128"\n      - "10.0.0.0/8"\n      - "172.16.0.0/12"\n      - "192.168.0.0/16"|' config/docker.yaml

# The rewrite above matches on exact indentation, so a reformat of config.yaml
# would silently ship an image whose dashboard 403s. Fail the build instead.
if ! grep -q '^      - "172.16.0.0/12"$' config/docker.yaml; then
	echo "generate-container-config: dashboard CIDR widening did not apply - check the allowed_cidrs block in config/config.yaml" >&2
	exit 1
fi

# The upstream comment tells operators to widen the CIDRs themselves; in this
# flavour that is already done, so replace it rather than ship a contradiction.
sed -i '/^# Loopback-only default matches the bare-metal first-run; Docker operators$/,/^# non-IP "localhost" needs to appear in allowed_hosts\.$/c\
# allowed_cidrs is pre-widened to the private ranges below so the dashboard\
# loads through a published port without extra configuration. Tighten it here\
# if the host sits on an untrusted LAN. IP-literal Hosts (127.0.0.1, [::1]) are\
# auto-accepted, so only the non-IP "localhost" needs to appear in allowed_hosts.' config/docker.yaml