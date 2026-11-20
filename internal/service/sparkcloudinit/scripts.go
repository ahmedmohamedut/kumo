package sparkcloudinit

// The first-boot scripts. They are constants: everything a request supplies
// reaches the node as a file the seed writes (spark-defaults.conf and
// kumo-node.env), so there is nothing here a name or an address could break
// out of. The image's units read those files (packaging/images/spark-4.3).
//
// No line is indented with a tab, so the scripts stay representable as YAML
// literal blocks.

// shredSeed removes cloud-init's copies of the seed, which carry the secret.
// /etc/spark keeps its own copy, readable by spark and root only, which the
// daemons need on every boot.
const shredSeed = `find /var/lib/cloud -type f \
  \( -name 'user-data*' -o -name 'vendor-data*' -o -name 'obj.pkl' \) \
  -exec shred -u {} + 2>/dev/null || true
`

// masterScript is a master's first boot: the master, and the history server
// when the cluster has an event-log bucket. kumod waits for the master's
// /json/ from outside (section 4, step 3).
const masterScript = `#!/usr/bin/env bash
# First boot of a Kumo Spark master (docs/design/09-spark.md section 4).
# Rendered by internal/service/sparkcloudinit; not edited by hand.
set -euo pipefail
export LC_ALL=C
say() { printf '%s\n' "$*" >/dev/console; }
trap 'journalctl --no-pager -n 30 -u spark-master -u spark-history >/dev/console 2>&1 || true; say ""; say "` + markerFailed + `"' ERR

. ` + nodeEnvPath + `
systemctl enable --now spark-master
if [ "${KUMO_SPARK_HISTORY:-0}" = 1 ]; then
  systemctl enable --now spark-history
fi

say ""
say "` + markerMasterReady + `"
` + shredSeed

// workerScript is a worker's first boot. The worker registers with the master
// on its own, retrying until it answers, so a worker may boot first.
const workerScript = `#!/usr/bin/env bash
# First boot of a Kumo Spark worker (docs/design/09-spark.md section 4).
# Rendered by internal/service/sparkcloudinit; not edited by hand.
set -euo pipefail
export LC_ALL=C
say() { printf '%s\n' "$*" >/dev/console; }
trap 'journalctl --no-pager -n 30 -u spark-worker >/dev/console 2>&1 || true; say ""; say "` + markerFailed + `"' ERR

systemctl enable --now spark-worker

say ""
say "` + markerWorkerReady + `"
` + shredSeed
