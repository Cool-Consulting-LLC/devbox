#!/bin/bash
# devbox-bootstrap: run once per instance by cloud-init. Replace tier.
# Gets the box to the point where it can run provision.sh from the pet disk.
set -euo pipefail
exec > >(tee -a /var/log/devbox-bootstrap.log) 2>&1

DEV=/dev/disk/by-id/google-code
MD=http://metadata.google.internal/computeMetadata/v1

for _ in $(seq 60); do [ -e "$DEV" ] && break; sleep 2; done
[ -e "$DEV" ] || { echo "code disk $DEV never appeared" >&2; exit 1; }

# Format on first boot only. A disk with any filesystem signature is never touched.
if ! blkid "$DEV" >/dev/null 2>&1; then
  mkfs.ext4 -q -L devbox-code -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$DEV"
fi
install -d /srv/code
grep -q ' /srv/code ' /etc/fstab || \
  echo "$DEV /srv/code ext4 defaults,nofail,discard 0 2" >> /etc/fstab
mountpoint -q /srv/code || mount /srv/code

# The guest agent creates this group for OS Login admins; make sure it exists first.
groupadd -f google-sudoers
chgrp google-sudoers /srv/code
chmod 2775 /srv/code
install -d -m 0755 -o root -g root /srv/code/.devbox

# Google's apt repositories: gcloud and the Ops Agent.
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
  | gpg --batch --yes --dearmor -o /etc/apt/keyrings/cloud.google.gpg
. /etc/os-release
cat > /etc/apt/sources.list.d/google-cloud.list <<EOF
deb [signed-by=/etc/apt/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main
deb [signed-by=/etc/apt/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt google-cloud-ops-agent-${VERSION_CODENAME}-all main
EOF
for i in 1 2 3; do
  apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q google-cloud-cli google-cloud-ops-agent && break
  sleep $((i * 10))
done

# The canonical provision.sh ships in instance metadata; the disk copy is a cache.
curl -fsS -H 'Metadata-Flavor: Google' "$MD/instance/attributes/devbox-provision" \
  -o /srv/code/.devbox/provision.sh.new
install -m 0755 /srv/code/.devbox/provision.sh.new /srv/code/.devbox/provision.sh
rm -f /srv/code/.devbox/provision.sh.new

systemctl daemon-reload
systemctl enable --now devbox-ssh-agent.service
# The relay's script is written by provision.sh, which starts it.
systemctl enable devbox-ssh-agent-relay.service
systemctl restart ssh.service
systemctl enable devbox-provision.service
systemctl start devbox-provision.service
