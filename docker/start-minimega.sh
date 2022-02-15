#!/bin/bash

/usr/share/openvswitch/scripts/ovs-ctl start

/opt/minimega/bin/miniweb -root=/opt/minimega/misc/web -addr=0.0.0.0:9001 &

: "${MM_BASE:=/tmp/minimega}"
: "${MM_FILEPATH:=/tmp/minimega/files}"
: "${MM_BROADCAST:=255.255.255.255}"
: "${MM_PORT:=9000}"
: "${MM_DEGREE:=2}"
: "${MM_CONTEXT:=minimega}"
: "${MM_LOGLEVEL:=info}"
: "${MM_LOGFILE:=/var/log/minimega.log}"

[[ -f "/etc/default/minimega" ]] && source "/etc/default/minimega"

/opt/minimega/bin/minimega \
  -force \
  -nostdin \
  -base=${MM_BASE} \
  -filepath=${MM_FILEPATH} \
  -broadcast=${MM_BROADCAST} \
  -port=${MM_PORT} \
  -degree=${MM_DEGREE} \
  -context=${MM_CONTEXT} \
  -level=${MM_LOGLEVEL} \
  -logfile=${MM_LOGFILE}
