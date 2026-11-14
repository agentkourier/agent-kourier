// Package docsonly holds the test that keeps the docs-only gate of the workflows the same in every job that has it: the
// checkout that fetches the merge commit's parents, the step that runs hack/ci/docs-only.sh, and the gate on every later
// step that a pull request runs. The gate is copied into each job, so a copy that drifts (one that loses
// continue-on-error, say) would break its failing open without failing anything else.
package docsonly
