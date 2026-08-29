#!/bin/bash
# Stop the instance after 90 minutes with no SSH session and no HTTP request.
#
# Install to /usr/local/bin/idle-stop.sh, mode 755, driven by /etc/cron.d/idle-stop:
#
#     */10 * * * * root /usr/local/bin/idle-stop.sh
#
THRESHOLD=90
MARKER=/var/log/platform-activity

# A missing marker means "start counting from now", not "idle since 1970".
#
# The previous version defaulted the timestamp to 0 when the file was absent, so the first
# cron tick after a fresh boot computed an idle time of roughly 29 million minutes and shut
# the instance down within ten minutes of it being started. Starting the host and finding it
# stopped again a few minutes later looked like a budget action or a crash.
[ -f "$MARKER" ] || touch "$MARKER"

# Idle time cannot exceed uptime.
#
# Touching a missing marker fixed the absent case but not the stale one, and the stale case
# is the normal one: the marker lives on the persistent disk, so it survives a stop. After
# the host sat stopped for two days, the marker was two days old, and the first cron tick
# after boot read "idle for 2880m" and powered the machine off — again within ten minutes,
# and again looking like something other than what it was. Whatever the marker says, the
# machine cannot have been idle for longer than it has been running.
UPTIME_MIN=$(( $(cut -d. -f1 /proc/uptime) / 60 ))
MARKER_MIN=$(( ( $(date +%s) - $(stat -c %Y "$MARKER") ) / 60 ))
LAST=$(( MARKER_MIN < UPTIME_MIN ? MARKER_MIN : UPTIME_MIN ))

# `who` only reports sessions that allocate a TTY. An SSH command run without a PTY — which
# is how every scripted deploy connects — and a port-forward (`ssh -N`) both leave no utmp
# entry, so `who` showed an empty machine while it was being actively worked on. Counting
# established connections on port 22 catches both.
if who | grep -q . \
   || ss -Htn state established '( sport = :22 )' | grep -q . \
   || ss -Htn state established '( sport = :8080 or sport = :3000 )' | grep -q .; then
  touch "$MARKER"
  exit 0
fi

# Traffic through the tunnel is invisible to every check above.
#
# cloudflared dials outward and forwards to Caddy inside the cluster, so a reviewer using
# Grafana from the other side of the world produces no inbound connection and no listening
# host port. The checks above would call that machine idle and stop it mid-session, which
# is the worst possible moment and the hardest to explain.
#
# Caddy logs every request it serves, so ask it. The dashboard's own /api/status poll is
# excluded deliberately: it fires roughly once a minute for as long as a tab is open, so
# counting it would mean a single forgotten tab keeps the instance running — and billing —
# indefinitely. Excluding it, a passive tab lets the host idle out normally while any real
# interaction, on any of the three hostnames, holds it up.
# Filter, then test for a remainder. The obvious `grep -qv` is not written here on purpose:
# GNU grep returns 0 when a non-matching line exists, BSD grep does not, so the same
# pipeline reports "idle" on one platform and "active" on the other. This form means the
# same thing everywhere.
if k3s kubectl -n edge logs deploy/caddy --since=11m 2>/dev/null \
     | grep 'handled request' | grep -v '/api/status' | grep -q .; then
  touch "$MARKER"
  exit 0
fi

if [ "$LAST" -ge "$THRESHOLD" ]; then
  logger "idle for ${LAST}m, stopping instance to preserve budget"
  shutdown -h now
fi
