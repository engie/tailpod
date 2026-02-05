#!/usr/bin/env sh

LEASES_FILE="/var/db/dhcpd_leases"
MAC_RAW="02:42:ac:11:00:01"

MAC=$(printf '%s' "$MAC_RAW" | tr '[:upper:]' '[:lower:]' | sed -E 's/(^|:)0([0-9a-f])/\1\2/g')

IP=$(awk -v mac="$MAC" '
  /^\{/ { ip=""; hw="" }
  /ip_address=/ {
    line=$0
    sub(/.*ip_address=/,"",line)
    ip=line
  }
  /hw_address=1,/ {
    line=$0
    sub(/.*hw_address=1,/,"",line)
    hw=line
  }
  /^\}/ {
    if (hw == mac && ip != "") {
      print ip
      exit
    }
  }
' "$LEASES_FILE")

if [ -z "$IP" ]; then
  echo "No lease found for $MAC_RAW" >&2
  exit 1
fi

echo "$IP"

SSH_USER=${SSH_USER:-core}
SSH_ERR=$(
  ssh -o BatchMode=yes \
      -o ConnectTimeout=2 \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR \
      -p 22 \
      "$SSH_USER@$IP" exit 2>&1
)
SSH_STATUS=$?

if [ $SSH_STATUS -ne 0 ]; then
  case "$SSH_ERR" in
    *"Permission denied"*|*"No supported authentication methods available"* )
      ;;
    * )
      echo "No TCP 22 response from $IP" >&2
      [ -n "$SSH_ERR" ] && echo "$SSH_ERR" >&2
      exit 2
      ;;
  esac
fi
