#!/bin/sh
# Builds the service images with in-cluster Kaniko and deploys them.
#
# Skaffold's Kaniko builder uploads the build context through `kubectl exec`,
# which agent workers may not use here, so this passes the context to the
# builder Pods as a ConfigMap instead.
#
# Usage: deploy/build.sh [service...]   (default: all services)
# Environment: KUBE_CONTEXT, NAMESPACE, REPO (registry repository prefix)
set -eu
cd "$(dirname "$0")/.."

: "${KUBE_CONTEXT:?set KUBE_CONTEXT}"
: "${NAMESPACE:=demo}"
: "${REPO:=${SKAFFOLD_DEFAULT_REPO:?set REPO or SKAFFOLD_DEFAULT_REPO}}"
k() { kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"; }

services=${*:-indexer catalog streamer players frontend transcoder}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Use the same explicit build inputs as the local Docker allowlist. Reading
# existing paths also supports uncommitted source removals during development.
for svc in $services; do
  case $svc in
    indexer | catalog | streamer | players | frontend | transcoder) ;;
    *) echo "unknown service: $svc" >&2; exit 1 ;;
  esac
done
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
  -czf "$work/context.tar.gz" .dockerignore go.mod go.sum cmd internal web build
tag=$(sha256sum "$work/context.tar.gz" | cut -c1-16)
k create configmap "slopify-build-$tag" --from-file=context.tar.gz="$work/context.tar.gz" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

for svc in $services; do
  pod="kaniko-slopify-$svc-$(echo "$tag" | cut -c1-8)"
  k delete pod "$pod" --ignore-not-found >/dev/null
  cat > "$work/$svc.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    skaffold-kaniko: skaffold-kaniko
    app.kubernetes.io/name: slopify-build
spec:
  restartPolicy: Never
  containers:
    - name: kaniko
      image: ghcr.io/osscontainertools/kaniko:v1.28.5
      args:
        - --context=tar:///context/context.tar.gz
        - --dockerfile=build/$svc.Dockerfile
        - --destination=$REPO/slopify-$svc:$tag
        - --cache=true
        - --cache-repo=$REPO/slopify-cache
        - --digest-file=/dev/termination-log
      resources:
        requests:
          cpu: "1"
          memory: 1Gi
        limits:
          cpu: "4"
          memory: 4Gi
      volumeMounts:
        - name: context
          mountPath: /context
        - name: docker-config
          mountPath: /kaniko/.docker
  volumes:
    - name: context
      configMap:
        name: slopify-build-$tag
    - name: docker-config
      secret:
        secretName: zot-registry-cluster-push-kaniko
EOF
  k apply -f "$work/$svc.yaml" >/dev/null
  echo "building $svc in $pod"
done

for svc in $services; do
  pod="kaniko-slopify-$svc-$(echo "$tag" | cut -c1-8)"
  while phase=$(k get pod "$pod" -o jsonpath='{.status.phase}'); [ "$phase" = Pending ] || [ "$phase" = Running ]; do
    sleep 5
  done
  if [ "$phase" != Succeeded ]; then
    echo "build of $svc ended in phase $phase:" >&2
    k logs "$pod" --tail=40 >&2
    exit 1
  fi
  digest=$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.message}')
  echo "slopify-$svc=$REPO/slopify-$svc:$tag@$digest" >> "$work/images"
  k delete pod "$pod" --wait=false >/dev/null
done
k delete configmap "slopify-build-$tag" --wait=false >/dev/null

if [ "$#" -eq 0 ]; then
  # A full build deploys every service.
  set --
  while IFS== read -r name ref; do
    set -- "$@" --set-string "images.${name#slopify-}=$ref"
  done < "$work/images"
  helm template slopify chart --namespace "$NAMESPACE" \
    -f chart/values-demo.yaml "$@" | k apply -f -
else
  # Partial builds only roll the rebuilt Deployments.
  while IFS== read -r name ref; do
    if [ "$name" = slopify-transcoder ]; then
      k set image deployment -l app.kubernetes.io/name=slopify,app.kubernetes.io/component=transcoder "transcoder=$ref"
    else
      k set image "deployment/$name" "${name#slopify-}=$ref"
    fi
  done < "$work/images"
fi
cat "$work/images"
