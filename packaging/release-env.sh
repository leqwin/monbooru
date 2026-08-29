# Shared by the release workflow's build jobs: the version and the ldflags
# every artifact is stamped with. Sourced, not run.
VERSION=$(tr -d '[:space:]' < VERSION.md)
REPO=$(tr -d '[:space:]' < REPOSITORY.md)
DOC=$(tr -d '[:space:]' < DOC.md)
MOD="github.com/monbooru/monbooru/internal/web"
LDFLAGS="-X '$MOD.Version=$VERSION' -X '$MOD.RepoURL=$REPO' -X '$MOD.DocURL=$DOC'"
