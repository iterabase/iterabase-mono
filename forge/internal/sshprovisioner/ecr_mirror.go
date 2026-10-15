package sshprovisioner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ECRMirror routes a hosted node's ghcr.io pulls through the hosted account's
// ECR pull-through cache (DES-HOR-593-07).
type ECRMirror struct {
	// Registry is the ECR registry host, <account>.dkr.ecr.<region>.amazonaws.com.
	Registry string
	// Prefix is the pull-through cache repository prefix (for example ghcr).
	Prefix string
	Region string
}

var (
	ecrRegistryRe = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com$`)
	ecrPrefixRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	awsRegionRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

// ParseECRMirror splits the contract's image-registry-prefix
// (<registry>/<prefix>) and validates every part, since they are written into
// host files.
func ParseECRMirror(imageRegistryPrefix, region string) (ECRMirror, error) {
	registry, prefix, ok := strings.Cut(imageRegistryPrefix, "/")
	m := ECRMirror{Registry: registry, Prefix: prefix, Region: region}
	if !ok || !ecrRegistryRe.MatchString(registry) || !ecrPrefixRe.MatchString(prefix) || !awsRegionRe.MatchString(region) {
		return ECRMirror{}, fmt.Errorf("image-registry-prefix %q (region %q) is not <account>.dkr.ecr.<region>.amazonaws.com/<prefix>", imageRegistryPrefix, region)
	}
	return m, nil
}

const (
	ecrMirrorScriptPath = "/usr/local/sbin/iterabase-ecr-mirror"
	ecrMirrorUnit       = "iterabase-ecr-mirror"
	// k3s regenerates this directory from registries.yaml on every start, so
	// the refresher also runs as a k3s ExecStartPost.
	ghcrHostsDir = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d/ghcr.io"
)

// EnsureECRMirror installs the refresher that writes containerd's ghcr.io
// hosts.toml: the ECR cache first (override_path to /v2/<prefix>), ghcr.io as
// the fallback server, with a fresh ECR token from the node role as the
// mirror's Authorization header. A timer refreshes it every 6 hours (tokens
// last 12) and a k3s drop-in re-writes it after every k3s start. It runs once
// immediately and is idempotent; it may run before k3s is installed.
func (p *SSHProvisioner) EnsureECRMirror(ctx context.Context, m ECRMirror) error {
	files := []struct {
		path, mode, content string
	}{
		{ecrMirrorScriptPath, "0755", ecrMirrorScript(m)},
		{"/etc/systemd/system/" + ecrMirrorUnit + ".service", "0644", ecrMirrorService},
		{"/etc/systemd/system/" + ecrMirrorUnit + ".timer", "0644", ecrMirrorTimer},
		{"/etc/systemd/system/k3s.service.d/10-iterabase-ecr-mirror.conf", "0644", ecrMirrorK3sDropIn},
	}
	for _, f := range files {
		cmd := "sudo install -d -m 0755 " + shellQuote(dirOf(f.path)) +
			" && sudo tee " + shellQuote(f.path+".tmp") + " >/dev/null" +
			" && sudo chmod " + f.mode + " " + shellQuote(f.path+".tmp") +
			" && sudo mv " + shellQuote(f.path+".tmp") + " " + shellQuote(f.path)
		if _, err := p.runStdin(ctx, cmd, f.content); err != nil {
			return fmt.Errorf("install %s: %w", f.path, err)
		}
	}
	if _, err := p.run(ctx, "sudo systemctl daemon-reload && sudo systemctl enable --now "+ecrMirrorUnit+".timer && sudo systemctl start "+ecrMirrorUnit+".service"); err != nil {
		return fmt.Errorf("start ECR mirror refresher: %w", err)
	}
	return nil
}

func dirOf(path string) string {
	return path[:strings.LastIndex(path, "/")]
}

// ecrMirrorScript fetches the instance-role credentials over IMDSv2 (the host
// itself, so hop limit 1 does not apply), signs ecr:GetAuthorizationToken with
// curl's SigV4, and atomically writes hosts.toml. Credentials reach curl
// through a 0600 config file, never its argument list.
func ecrMirrorScript(m ECRMirror) string {
	return fmt.Sprintf(`#!/bin/bash
# Installed by forge (DES-HOR-593-07). Routes ghcr.io pulls through the hosted
# ECR pull-through cache with a token from this node's role.
set -euo pipefail
umask 077
REGISTRY=%q
PREFIX=%q
REGION=%q
HOSTS_DIR=%q

imds() { curl -fsS --max-time 5 -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" "http://169.254.169.254/latest/$1"; }
IMDS_TOKEN=$(curl -fsS --max-time 5 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 300" http://169.254.169.254/latest/api/token)
ROLE=$(imds meta-data/iam/security-credentials/)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
imds "meta-data/iam/security-credentials/$ROLE" > "$work/creds.json"
python3 - "$work/creds.json" "$work/curl.conf" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
with open(sys.argv[2], "w") as f:
    f.write('user = "%%s:%%s"\n' %% (c["AccessKeyId"], c["SecretAccessKey"]))
    f.write('header = "x-amz-security-token: %%s"\n' %% c["Token"])
PY
curl -fsS --max-time 20 -K "$work/curl.conf" \
  --aws-sigv4 "aws:amz:${REGION}:ecr" \
  -H "Content-Type: application/x-amz-json-1.1" \
  -H "X-Amz-Target: AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" \
  -d '{}' "https://api.ecr.${REGION}.amazonaws.com/" > "$work/token.json"
TOKEN=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["authorizationData"][0]["authorizationToken"])' "$work/token.json")
install -d -m 0700 "$HOSTS_DIR"
cat > "$HOSTS_DIR/hosts.toml.tmp" <<EOF
server = "https://ghcr.io"

[host."https://${REGISTRY}/v2/${PREFIX}"]
  capabilities = ["pull", "resolve"]
  override_path = true
  [host."https://${REGISTRY}/v2/${PREFIX}".header]
    Authorization = ["Basic ${TOKEN}"]
EOF
chmod 0600 "$HOSTS_DIR/hosts.toml.tmp"
mv "$HOSTS_DIR/hosts.toml.tmp" "$HOSTS_DIR/hosts.toml"
`, m.Registry, m.Prefix, m.Region, ghcrHostsDir)
}

const ecrMirrorService = `[Unit]
Description=Refresh the ghcr.io ECR pull-through mirror token (forge, DES-HOR-593-07)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=` + ecrMirrorScriptPath + `
`

const ecrMirrorTimer = `[Unit]
Description=Refresh the ghcr.io ECR pull-through mirror token every 6 hours

[Timer]
OnBootSec=1min
OnUnitActiveSec=6h
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
`

// The leading "-" keeps a failed refresh from failing k3s itself; pulls then
// fall back to ghcr.io until the timer succeeds.
const ecrMirrorK3sDropIn = `[Service]
ExecStartPost=-` + ecrMirrorScriptPath + `
`
