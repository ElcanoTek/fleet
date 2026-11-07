#!/usr/bin/env bash
# scripts/slsa-predicate.sh TAG COMMIT — the SLSA v1 build-provenance predicate
# for a fleet release (ADR-0100), on stdout.
#
# Why not the predicate actions/attest-build-provenance generates: it takes the
# source commit from the job's OIDC token, which under release.yml's
# `workflow_run` trigger is the DEFAULT BRANCH HEAD when the release ran — not
# the commit the release was built from. Two pushes close together, and the
# provenance would name a later commit than the one in the binary. So the
# publish job writes this predicate, in the same shape as that action's
# (buildType https://actions.github.io/buildtypes/workflow/v1), with the
# release commit as the source:
#
#   resolvedDependencies[0]  git+<repo>@refs/tags/TAG at COMMIT — what was built
#                            (the build job checks the tag resolves to it);
#   resolvedDependencies[1]  git+<repo>@<workflow ref> at GITHUB_SHA — the
#                            trusted default-branch code that ran the workflow.
#
# The publish job then verifies the attestation it made and requires
# resolvedDependencies[0] to name COMMIT (scripts/check-attested-commit.sh).
# Reads the GitHub Actions environment (GITHUB_SERVER_URL, GITHUB_REPOSITORY,
# GITHUB_REF, GITHUB_SHA, GITHUB_WORKFLOW_REF, GITHUB_EVENT_NAME,
# GITHUB_REPOSITORY_ID, GITHUB_REPOSITORY_OWNER_ID, RUNNER_ENVIRONMENT,
# GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT). Values are passed to jq as data.
set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 TAG COMMIT" >&2; exit 2; }
tag="$1" commit="$2"
[[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$ ]] || { echo "not a release tag: $tag" >&2; exit 2; }
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "not a commit SHA: $commit" >&2; exit 2; }
for v in GITHUB_SERVER_URL GITHUB_REPOSITORY GITHUB_REF GITHUB_SHA GITHUB_WORKFLOW_REF GITHUB_EVENT_NAME \
  GITHUB_REPOSITORY_ID GITHUB_REPOSITORY_OWNER_ID GITHUB_RUN_ID GITHUB_RUN_ATTEMPT; do
  [[ -n "${!v:-}" ]] || { echo "$v is not set (run inside GitHub Actions)" >&2; exit 2; }
done
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

# resolvedDependencies[0] MUST be the signing workflow source: GitHub's
# attestation API rejects a predicate whose first dependency differs from the
# Fulcio certificate (SourceRepositoryURI@SourceRepositoryRef and the token's
# sha) -- under workflow_run, the default branch that ran release.yml at
# GITHUB_SHA. [1] is the release itself: the tag at the commit that was built
# (scripts/check-attested-commit.sh reads the release commit from it).
jq -n \
  --arg server "$GITHUB_SERVER_URL" --arg repo "$GITHUB_REPOSITORY" \
  --arg ref "$GITHUB_REF" --arg sha "$GITHUB_SHA" --arg wref "$GITHUB_WORKFLOW_REF" \
  --arg event "$GITHUB_EVENT_NAME" --arg repo_id "$GITHUB_REPOSITORY_ID" \
  --arg owner_id "$GITHUB_REPOSITORY_OWNER_ID" --arg runner_env "${RUNNER_ENVIRONMENT:-github-hosted}" \
  --arg run_id "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" \
  --arg tag "$tag" --arg commit "$commit" '
  ($wref | sub("^" + $repo + "/"; "") | split("@")[0]) as $path
  | {
      buildDefinition: {
        buildType: "https://actions.github.io/buildtypes/workflow/v1",
        externalParameters: {
          workflow: { ref: $ref, repository: ($server + "/" + $repo), path: $path },
          release: { tag: $tag, commit: $commit }
        },
        internalParameters: {
          github: {
            event_name: $event, repository_id: $repo_id,
            repository_owner_id: $owner_id, runner_environment: $runner_env
          }
        },
        resolvedDependencies: [
          { uri: ("git+" + $server + "/" + $repo + "@" + $ref), digest: { gitCommit: $sha },
            annotations: { role: "workflow definition (the default branch that ran release.yml)" } },
          { uri: ("git+" + $server + "/" + $repo + "@refs/tags/" + $tag), digest: { gitCommit: $commit },
            annotations: { role: "release source (the commit built and published)" } }
        ]
      },
      runDetails: {
        builder: { id: ($server + "/" + $wref) },
        metadata: { invocationId: ($server + "/" + $repo + "/actions/runs/" + $run_id + "/attempts/" + $attempt) }
      }
    }'
