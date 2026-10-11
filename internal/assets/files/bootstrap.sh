#!/bin/bash
# devbox-bootstrap: run once per instance by cloud-init. Replace tier.
# Gets the box to the point where devbox-provision.service can take over.
set -euo pipefail
exec > >(tee -a /var/log/devbox-bootstrap.log) 2>&1

DEV=/dev/disk/by-id/google-code

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
# Sticky: enrolled users write here, and must not rename root's .devbox away.
# No access for others: only enrolled admins, through their ACL, see projects.
chmod 3770 /srv/code
install -d -m 0755 -o root -g root /srv/code/.devbox

# Google's apt repositories: gcloud and the Ops Agent. The key is pinned by
# fingerprint, as provision.sh pins its own: TLS alone trusts whoever answers.
install -d -m 0755 /etc/apt/keyrings
key=$(mktemp)
curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg -o "$key"
got=$(gpg --show-keys --with-colons "$key" 2>/dev/null | awk -F: '$1=="pub"{p=1} $1=="fpr"&&p{print $10; p=0}')
if [ "$got" != 35BAA0B33E9EB396F59CA838C0BA5CE6DC6315A3 ]; then
  echo "Google apt key fingerprint mismatch ($got); refusing" >&2; exit 1
fi
gpg --batch --yes --dearmor -o /etc/apt/keyrings/cloud.google.gpg "$key"
rm -f "$key"
. /etc/os-release
cat > /etc/apt/sources.list.d/google-cloud.list <<EOF
deb [signed-by=/etc/apt/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main
deb [signed-by=/etc/apt/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt google-cloud-ops-agent-${VERSION_CODENAME}-all main
EOF
for i in 1 2 3; do
  apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q google-cloud-cli google-cloud-ops-agent && break
  sleep $((i * 10))
done

# devbox-provision.service fetches provision.sh from metadata itself, every boot.
systemctl daemon-reload
systemctl enable --now devbox-ssh-agent.service
# The relay's script is written by provision.sh, which starts it.
systemctl enable devbox-ssh-agent-relay.service
systemctl restart ssh.service
systemctl enable devbox-provision.service
systemctl start devbox-provision.service
