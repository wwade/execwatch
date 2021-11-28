#!/bin/bash
path=$(dirname "$0")
cache=$1
log=${cache}.log
set -x
"${path}/execwatch" "$@" >>"$log" 2>&1 &
disown
