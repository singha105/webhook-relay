#!/usr/bin/env bash
#
# Exits 0 if TCP <port> is being listened on, 1 if it is free.
#
# Detection is genuinely awkward to do portably:
#   * lsof only shows sockets owned by the calling user unless run as root, so
#     it silently misses a listener belonging to another account. That is not
#     hypothetical — it is exactly how this check first passed a port that was
#     actually occupied.
#   * netstat is absent from most minimal Linux images, including the GitHub
#     Actions Ubuntu runner, where it would fail open and check nothing.
#   * ss is the modern Linux replacement but is not on macOS.
#
# So: try each in turn, and fall back to actually attempting a bind, which is
# the only method that is both universally available and always correct.
#
# Note the deliberate absence of `set -o pipefail` and of `grep -q`. Together
# they produce a false negative: grep -q exits on the first match, the upstream
# tool takes SIGPIPE, and pipefail reports the whole pipeline as failed — so a
# port that IS in use gets reported as free. Output is captured instead.
set -u

port="${1:?usage: port-in-use.sh <port>}"

if command -v ss >/dev/null 2>&1; then
  found=$(ss -ltnH 2>/dev/null | awk '{print $4}' | grep -E "[:.]${port}$" || true)
  [ -n "$found" ] && exit 0
  exit 1
fi

if command -v netstat >/dev/null 2>&1; then
  found=$(netstat -an 2>/dev/null | grep -E "[.:]${port}[[:space:]]+.*LISTEN" || true)
  [ -n "$found" ] && exit 0
  exit 1
fi

# Last resort: try to bind it. Requires python3, which every platform we
# support already has.
if command -v python3 >/dev/null 2>&1; then
  python3 - "$port" <<'PY'
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("0.0.0.0", int(sys.argv[1])))
except OSError:
    sys.exit(0)   # in use
finally:
    s.close()
sys.exit(1)       # free
PY
  exit $?
fi

# No detector available. Fail open rather than blocking a valid start, but say
# so — a silent no-op check is worse than no check.
echo "  note: cannot check port ${port} (no ss, netstat, or python3); skipping preflight" >&2
exit 1
