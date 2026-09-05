#!/usr/bin/env bash
#
# Generate a coverage badge as a committed SVG.
#
# Deliberately not Codecov or Coveralls: both need an account, and the project
# constraint is that GitHub is the only external service. A self-contained SVG
# in the repo has no dependency at all -- it renders offline, in a fork, and in
# a tarball.
#
# The cost is that it is a snapshot, not live. `make cover` regenerates it, and
# CI fails if it is stale (see .github/workflows/ci.yml).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

PROFILE="${1:-coverage.out}"
OUT="${2:-docs/coverage/badge.svg}"
[ -f "$PROFILE" ] || { echo "coverage-badge: no $PROFILE; run go test -coverprofile first" >&2; exit 1; }

PCT=$(go tool cover -func="$PROFILE" | tail -1 | awk '{gsub(/%/,"",$NF); print $NF}')
INT=${PCT%.*}

# Colour thresholds match what a reader expects from shields.io, so the badge
# does not quietly imply the number is better than it is.
if   [ "$INT" -ge 90 ]; then COLOR="#4c1"
elif [ "$INT" -ge 75 ]; then COLOR="#97ca00"
elif [ "$INT" -ge 60 ]; then COLOR="#a4a61d"
elif [ "$INT" -ge 40 ]; then COLOR="#dfb317"
elif [ "$INT" -ge 20 ]; then COLOR="#fe7d37"
else                         COLOR="#e05d44"; fi

LABEL="coverage"
VALUE="${PCT}%"
# 6px per char + padding, close enough for the 11px DejaVu Sans the badge uses.
LW=$(( ${#LABEL} * 6 + 10 ))
VW=$(( ${#VALUE} * 6 + 10 ))
TW=$(( LW + VW ))

mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<SVG
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="${TW}" height="20" role="img" aria-label="${LABEL}: ${VALUE}">
  <title>${LABEL}: ${VALUE}</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="${TW}" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="${LW}" height="20" fill="#555"/>
    <rect x="${LW}" width="${VW}" height="20" fill="${COLOR}"/>
    <rect width="${TW}" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="$(( LW * 10 / 2 ))" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="$(( (LW - 10) * 10 ))">${LABEL}</text>
    <text x="$(( LW * 10 / 2 ))" y="140" transform="scale(.1)" textLength="$(( (LW - 10) * 10 ))">${LABEL}</text>
    <text x="$(( (LW * 10) + (VW * 10 / 2) ))" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="$(( (VW - 10) * 10 ))">${VALUE}</text>
    <text x="$(( (LW * 10) + (VW * 10 / 2) ))" y="140" transform="scale(.1)" textLength="$(( (VW - 10) * 10 ))">${VALUE}</text>
  </g>
</svg>
SVG
echo "  wrote $OUT (${VALUE})"
