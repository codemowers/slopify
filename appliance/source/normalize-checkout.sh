#!/bin/sh
# Git records file executability, but checkout permissions inherit the host umask.
# Only public tracked source files and their parent directories are normalized;
# generated signing keys and other untracked private files are untouched.
set -eu
for checkout in "$@"; do
 (
  cd "$checkout"
  chmod 0755 .
  git ls-files --stage -z | xargs -0 -r sh -eu -c '
   tab=$(printf "\t")
   for entry do
    mode=${entry%% *}
    path=${entry#*"$tab"}
    case "$mode" in
     100644) chmod 0644 -- "$path" ;;
     100755) chmod 0755 -- "$path" ;;
     *) continue ;;
    esac
    while [ "$path" != "${path%/*}" ]; do
     path=${path%/*}
     printf "%s\000" "$path"
    done
   done
  ' sh | sort -zu | xargs -0 -r chmod 0755 --
 )
done
