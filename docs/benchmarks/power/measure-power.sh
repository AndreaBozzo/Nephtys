#!/usr/bin/env bash
# measure-power.sh — measure edge-node wall power via a Shelly Plug S MTR Gen3.
#
# RUN FROM THE SAMPLING HOST, NOT THE PI: polling the meter from the device under
# test would perturb its own power draw (extra CPU + Wi-Fi traffic).
#
# Usage:  ./measure-power.sh <label> <duration_s> [sample_interval_s]
# Env:    SHELLY_IP  (default 192.168.1.104)
#
# Output: one CSV row on stdout + a human-readable breakdown on stderr.
# CSV: label,duration_s,n_samples,P_mean_from_energy_W,P_mean_from_samples_W,P_min_W,P_max_W,stddev_W,Wh

set -uo pipefail

SHELLY="${SHELLY_IP:-192.168.1.104}"
LABEL="${1:-unlabelled}"
DUR="${2:-600}"
INTERVAL="${3:-5}"

RPC="http://${SHELLY}/rpc/Switch.GetStatus?id=0"

fetch() { curl -s -m 5 "$RPC" 2>/dev/null; }
get_apower() { sed -n 's/.*"apower":\([0-9.-]*\).*/\1/p'; }
get_energy() { sed -n 's/.*"aenergy":{"total":\([0-9.-]*\).*/\1/p'; }

probe="$(fetch)"
if [ -z "$probe" ]; then
  echo "ERROR: Shelly unreachable at $SHELLY" >&2
  exit 1
fi

echo "== Measure '$LABEL' — ${DUR}s window, sample every ${INTERVAL}s ==" >&2

E0="$(printf '%s' "$probe" | get_energy)"
T0="$(date +%s)"

samples=""
n=0
end=$(( T0 + DUR ))
while [ "$(date +%s)" -lt "$end" ]; do
  p="$(fetch | get_apower)"
  if [ -n "$p" ]; then
    samples="$samples $p"
    n=$(( n + 1 ))
  fi
  sleep "$INTERVAL"
done

final="$(fetch)"
E1="$(printf '%s' "$final" | get_energy)"
T1="$(date +%s)"
ELAPSED=$(( T1 - T0 ))

# Average power from the integrated energy counter — the robust method (true mean
# over the window). P[W] = dWh / dt[h].
read -r P_ENERGY WH <<EOF
$(awk -v e0="$E0" -v e1="$E1" -v s="$ELAPSED" 'BEGIN{ wh=e1-e0; if(s>0) printf "%.3f %.4f", wh/(s/3600.0), wh; else print "0 0" }')
EOF

# Instantaneous-sample stats (for dispersion, not the headline mean).
read -r P_MEAN P_MIN P_MAX P_SD <<EOF
$(printf '%s\n' $samples | awk '
  { v[NR]=$1; s+=$1; if(NR==1||$1<mn) mn=$1; if(NR==1||$1>mx) mx=$1 }
  END{
    if(NR==0){ print "0 0 0 0"; exit }
    m=s/NR; for(i=1;i<=NR;i++) d+=(v[i]-m)^2;
    printf "%.3f %.3f %.3f %.3f", m, mn, mx, (NR>1? sqrt(d/(NR-1)) : 0)
  }')
EOF

# On some Shelly firmware the energy counter accumulates in blocks and is unreliable
# on short windows; report both methods and flag the divergence rather than trusting
# one blindly. On ~20-minute windows the two agree within a few percent.
DIVERG="$(awk -v a="$P_ENERGY" -v b="$P_MEAN" 'BEGIN{ if(b>0) printf "%.1f", 100*(a-b)/b; else print "n/a" }')"

{
  echo "  window       : ${ELAPSED}s  (${n} samples)"
  echo "  energy       : ${WH} Wh accumulated (${E0} -> ${E1})"
  echo "  P (samples)  : ${P_MEAN} W   [min ${P_MIN} / max ${P_MAX}, sd ${P_SD}]"
  echo "  P (energy)   : ${P_ENERGY} W   [${DIVERG}% vs samples]"
} >&2

echo "${LABEL},${ELAPSED},${n},${P_ENERGY},${P_MEAN},${P_MIN},${P_MAX},${P_SD},${WH}"
