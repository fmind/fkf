#!/bin/sh
# git-log-json.sh <since> <until> <root> [author...] — written into <base>/bin by
# `fkf init --preset personal`.
#
# Prints one JSON array of commits authored by you and committed (or rebased/cherry-picked) in
# the half-open window, reachable from any ref across every clone or linked worktree under
# <root>. The event clock is Git's committer date because that is the clock `git log` filters;
# mixing it with author date makes an ordinary rebase fail the exact-window contract.
#
# Authors are the identities you commit under. With none given the script uses `git config
# user.email` as seen from the base, which is the global identity — and silently not the one a
# repository overrides with its own `user.email`, so commits made under a work or noreply
# address would never be collected. Listing every identity on the run: line makes the
# selection explicit.
#
# Fields and records are NUL-delimited and assembled by jq. Git commit subjects may contain
# every non-NUL byte, including the ASCII unit/record separators, so using either as structure
# would silently truncate a valid subject and file an incomplete day.
set -eu

# FKF deliberately starts declared commands from / so mutable base layers cannot become an
# interpreter's implicit input. Resolve the installed helper's own base only for Git's pure
# date parser and config lookup; executable support still stays under trust-digested bin/.
case "$0" in
  */*) script_dir=${0%/*} ;;
  *) script_dir=. ;;
esac
script_base=$(CDPATH='' cd -P "$script_dir/.." && pwd -P)

since=${1:?usage: git-log-json.sh <since> <until> <root> [author...]}
until=${2:?usage: git-log-json.sh <since> <until> <root> [author...]}
roots=${3:?usage: git-log-json.sh <since> <until> <root>[:<root>...] [author...]}
shift 3

# Several roots, colon-separated, because clones rarely live under one parent: a personal tree,
# a work tree, and a courses tree are three roots and were three sources before this. The
# separator is ":" to match PATH, and a root that does not exist is still an error rather than
# a silent empty day.
#
# A leading ~ is expanded by hand rather than through eval, which would also run a $(...)
# found in a path. fkf substitutes {{home}} itself, so this only serves a hand-edited run: line.
checked=""
saved_ifs=$IFS
IFS=:
for root in $roots; do
  IFS=$saved_ifs
  case "$root" in "~"*) root="$HOME${root#\~}" ;; esac
  # A root that does not exist used to produce `[]`: find printed nothing, jq made an empty
  # array, and the day looked complete with zero commits. A wrong path is a configuration
  # error, and the only honest outcome is to fail the day and say where to look.
  [ -d "$root" ] || {
    echo "git-log-json.sh: $root is not a directory; point the third argument of run: at your clones" >&2
    exit 1
  }
  checked="$checked$root
"
  IFS=:
done
IFS=$saved_ifs

# Pin both bounds to midnight. git's approxidate parser fills a bare YYYY-MM-DD with the
# CURRENT time of day, so `git log --since=2026-08-21` run at 14:00 silently starts at 14:00 and
# the collected "day" shifts with whenever the sync happened — a base whose contents depend on
# the clock rather than on the date it claims. Already-timestamped input is left alone.
case "$since" in *T*) ;; *) since="${since}T00:00:00" ;; esac
case "$until" in *T*) ;; *) until="${until}T00:00:00" ;; esac

# Ask Git's own date parser for the exact epoch bounds. `git log --until` is inclusive, so its
# result is only a coarse candidate set; the final jq projection enforces fkf's [since, until)
# contract against %ct without teaching a second parser about Git's accepted date syntax.
since_epoch=$(git -C "$script_base" rev-parse --since="$since")
until_epoch=$(git -C "$script_base" rev-parse --until="$until")
since_epoch=${since_epoch#--max-age=}
until_epoch=${until_epoch#--min-age=}
case "${since_epoch}:${until_epoch}" in
  *[!0-9:]*)
    echo "git-log-json.sh: git could not resolve the requested time bounds" >&2
    exit 1
    ;;
  *) ;;
esac
[ "$since_epoch" -lt "$until_epoch" ] || {
  echo "git-log-json.sh: since must be before until" >&2
  exit 1
}

if [ "$#" -eq 0 ]; then
  if author=$(git -C "$script_base" config --get user.email); then
    :
  else
    status=$?
    [ "$status" -eq 1 ] || exit "$status"
    author=""
  fi
  [ -n "$author" ] || {
    echo "git-log-json.sh: git config user.email is unset and no author was given; append your identities to run:" >&2
    exit 1
  }
  set -- "$author"
fi
# Turn each identity into a --author flag; git ORs repeated --author flags. The loop rotates
# the positional parameters in place, which is the one list POSIX sh has.
for identity in "$@"; do
  set -- "$@" "--author=$identity"
  shift
done

# remote_key returns a provider-neutral host/path key for a URL or SCP-style remote. Userinfo
# and volatile query or fragment credentials are excluded. Malformed or local paths return
# empty so the caller can fall back to an opaque local identity.
remote_key() {
  candidate=$1
  candidate=${candidate%%\#*}
  candidate=${candidate%%\?*}
  case "$candidate" in
    *://*)
      scheme=${candidate%%://*}
      case "$scheme" in "" | [!A-Za-z]* | *[!A-Za-z0-9+.-]*) return 0 ;; esac
      authority_and_path=${candidate#*://}
      authority=${authority_and_path%%/*}
      case "$authority_and_path" in */*) [ -n "$authority" ] || return 0; path=${authority_and_path#*/} ;; *) return 0 ;; esac
      host_port=${authority##*@}
      case "$host_port" in
        *:*)
          host=${host_port%%:*}
          port=${host_port#*:}
          case "$port" in "" | *[!0-9]*) return 0 ;; esac
          [ "$port" -ge 1 ] 2>/dev/null && [ "$port" -le 65535 ] 2>/dev/null || return 0
          host_suffix=":$port"
          ;;
        *) host=$host_port; host_suffix="" ;;
      esac
      ;;
    *:*/*)
      scp_host=${candidate%%:*}
      case "$scp_host" in "" | */*) return 0 ;; esac
      host=${scp_host##*@}
      host_suffix=""
      path=${candidate#*:}
      ;;
    *) return 0 ;;
  esac
  host=$(printf '%s' "$host" | LC_ALL=C tr '[:upper:]' '[:lower:]')
  case "$host" in "" | .* | *. | *..* | *[!a-z0-9.-]*) return 0 ;; esac
  saved_ifs=$IFS
  IFS=.
  set -- $host
  IFS=$saved_ifs
  for label in "$@"; do
    case "$label" in "" | -* | *-) return 0 ;; esac
  done
  path=${path%.git}
  case "$path" in "" | /* | */ | *//* | *[!A-Za-z0-9._~+%@/-]*) return 0 ;; esac
  IFS=/
  set -- $path
  IFS=$saved_ifs
  [ "$#" -ge 2 ] || return 0
  for segment in "$@"; do
    case "$segment" in "" | . | ..) return 0 ;; esac
  done
  printf '%s%s/%s\n' "$host" "$host_suffix" "$path"
}

roots_file=$(mktemp); markers=$(mktemp); sorted_markers=$(mktemp)
gitdirs=$(mktemp); sorted_gitdirs=$(mktemp); log_output=$(mktemp); records=$(mktemp)
trap 'rm -f "$roots_file" "$markers" "$sorted_markers" "$gitdirs" "$sorted_gitdirs" "$log_output" "$records"' EXIT
printf '%s' "$checked" > "$roots_file"
while read -r root; do
  [ -n "$root" ] || continue
  # A normal checkout has a .git directory; a linked worktree has a .git file. Ask git for
  # the shared directory instead of parsing that file ourselves, then scan each repository
  # once even when several of its worktrees live below the configured roots. There is no depth
  # cap: a declared root means its complete tree. -H follows a root that is itself a symlink,
  # but descendant symlinks remain data, so loops cannot recurse. Pruning .git directories also
  # avoids walking repository databases after their marker has been found.
  find -H "$root" -type d -name .git -prune -print -o -type f -name .git -print
done < "$roots_file" > "$markers"
sort -u "$markers" > "$sorted_markers"
while read -r marker; do
  [ -n "$marker" ] || continue
  repo=$(dirname "$marker")
  git -C "$repo" rev-parse --path-format=absolute --git-common-dir
done < "$sorted_markers" > "$gitdirs"
sort -u "$gitdirs" > "$sorted_gitdirs"
: > "$records"
while read -r gitdir; do
  [ -n "$gitdir" ] || continue
  repo=$(dirname "$gitdir")
  if remote=$(git --git-dir="$gitdir" config --get remote.origin.url 2>/dev/null); then
    :
  else
    status=$?
    [ "$status" -eq 1 ] || exit "$status"
    remote=""
  fi
  key=$(remote_key "$remote")
  full=""
  case "$key" in
    github.com/*)
      github_path=${key#github.com/}
      case "$github_path" in
        */*/* | /* | */) ;;
        */*)
          owner=${github_path%%/*}
          name=${github_path#*/}
          case "$owner" in "" | . | .. | *[!A-Za-z0-9._-]*) ;;
            *) case "$name" in "" | . | .. | *[!A-Za-z0-9._-]*) ;; *) full=$github_path ;; esac ;;
          esac
          ;;
      esac
      ;;
  esac
  if [ -n "$full" ]; then
    repo_identity=$full
  else
    # The opaque digest keeps same-hash commits in distinct repositories distinct without
    # exposing a non-GitHub host, repository path, or local checkout path.
    if [ -n "$key" ]; then identity_key="remote:$key"; else identity_key="gitdir:$gitdir"; fi
    repo_digest=$(printf '%s\n' "$identity_key" | git -C "$script_base" hash-object --stdin)
    case "$repo_digest" in "" | *[!0-9a-f]*)
      echo "git-log-json.sh: git returned an invalid repository identity digest" >&2
      exit 1
    esac
    repo_identity="opaque:$repo_digest"
  fi
  # --all includes commits reachable only from an unmerged local or remote ref. Walking the
  # common repository once also prevents linked worktrees from multiplying identical rows.
	# Git interprets --author values as regular expressions unless --fixed-strings is set. An
	# identity is reviewed configuration, not a pattern: a dot in an email must match a dot.
  git --git-dir="$gitdir" log --all --fixed-strings "$@" \
    --since="$since" --until="$until" \
    --pretty=format:'%H%x00%ct%x00%aE%x00%s%x00' > "$log_output"
  jq -R -s --arg repo "$full" --arg repo_identity "$repo_identity" '
      if . == "" then []
      else split("\u0000")
        | if .[-1] != "" then error("git log output has no terminal NUL") else .[:-1] end
        | if length % 4 != 0 then error("git log output has an incomplete field group") else . end
      end
      | [range(0; length; 4) as $offset
          | { hash: (.[ $offset ] | ltrimstr("\n")), _epoch: (.[$offset + 1] | tonumber),
              author_email: .[$offset + 2], message: .[$offset + 3], repo_full: $repo,
              _repo_identity: $repo_identity }]
      | .[]' < "$log_output" >> "$records"
# A commit can be present in several clones. Give it one semantic identity per repository and
# collapse repeated clones before fkf verifies that every URI fragment names exactly one record.
done < "$sorted_gitdirs"
jq -s --argjson since "$since_epoch" --argjson until "$until_epoch" '
  def fkf_identity: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
    | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
  def participant_uri:
    ascii_downcase as $email
    | (try ($email | capture("^(?:[0-9]+\\+)?(?<login>[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)@users\\.noreply\\.github\\.com$")) catch null) as $github
    | if $github == null then "person:email/" + ($email | fkf_identity)
      else "actor:github.com/" + $github.login
      end;
  map(select(._epoch >= $since and ._epoch < $until)
    # Git versions spell a UTC %cI value as either Z or +00:00. Derive one canonical
    # representation from the epoch that also drives window filtering.
    | .time = (._epoch | todateiso8601)
    | . + {
        uid: (._repo_identity + "@" + .hash),
        repo_full: (if .repo_full == "" then null else .repo_full end),
        repository_uri: (if .repo_full == "" then null else ("repo:github.com/" + .repo_full) end),
        participant_uris: (if .author_email == "" then []
          else [(.author_email | participant_uri)] end)
      }
    | del(._epoch, ._repo_identity))
  | unique_by(.uid)
' < "$records"
