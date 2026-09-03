These are copied from the repository root `/migrations` by `make chart-sync`,
so the chart carries the exact migration set for the version it deploys.

They are duplicated rather than symlinked because `helm package` does not
follow symlinks out of the chart directory, and a chart that silently ships
zero migrations is worse than a duplicated file. `make chart-sync` is run by CI
and the check fails the build if the two directories diverge.
