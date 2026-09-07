#!/usr/bin/env bash
# Refuse release authority when the remote tag no longer names this workflow commit.
set -euo pipefail

: "${GITHUB_REF_NAME:?GITHUB_REF_NAME is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"

object="$(
  gh api "repos/${GITHUB_REPOSITORY}/git/ref/tags/${GITHUB_REF_NAME}" \
    --jq '.object | [.type, .sha] | @tsv'
)"
IFS=$'\t' read -r object_type object_sha <<<"${object}"
if [[ "${object_type}" == tag ]]; then
  object="$(
    gh api "repos/${GITHUB_REPOSITORY}/git/tags/${object_sha}" \
      --jq '.object | [.type, .sha] | @tsv'
  )"
  IFS=$'\t' read -r object_type object_sha <<<"${object}"
fi

if [[ "${object_type}" != commit ]]; then
  echo "release tag ${GITHUB_REF_NAME} resolves to unsupported object type ${object_type}" >&2
  exit 1
fi
if [[ "${object_sha}" != "${GITHUB_SHA}" ]]; then
  echo "release tag ${GITHUB_REF_NAME} does not resolve to workflow commit ${GITHUB_SHA}" >&2
  exit 1
fi

# A tag event can name an unmerged feature commit, so bind publication authority
# to the protected branch. An ancestor remains valid so a later main advance
# cannot strand an idempotent rerun after PyPI has already accepted the version.
main_object="$(
  gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" \
    --jq '.object | [.type, .sha] | @tsv'
)"
IFS=$'\t' read -r main_type main_sha <<<"${main_object}"
if [[ "${main_type}" != commit ]]; then
  echo "main resolves to unsupported object type ${main_type}" >&2
  exit 1
fi
if [[ "${main_sha}" != "${object_sha}" ]]; then
  ancestry="$(
    gh api "repos/${GITHUB_REPOSITORY}/compare/${object_sha}...${main_sha}" --jq '.status'
  )"
  if [[ "${ancestry}" != ahead ]]; then
    echo "release commit ${object_sha} is not contained in current main ${main_sha}" >&2
    exit 1
  fi
fi
