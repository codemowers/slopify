#!/bin/sh
# Explicitly select the public demo without changing the private skaffold.env.
set -eu
cd "$(dirname "$0")/../.."
export SKAFFOLD_KUBE_CONTEXT=admin@ee-lte-1.codemowers.io
export SKAFFOLD_NAMESPACE=demo
export SKAFFOLD_DEFAULT_REPO=zot.registry.ee-lte-1.codemowers.io/laurivosandi
if [ "$#" -eq 0 ]; then set -- run; fi
exec skaffold "$@" -p demo
