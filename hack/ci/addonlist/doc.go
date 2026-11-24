// Package addonlist holds the test that keeps the workflows' add-on legs to hack/addons.txt, the one list of the
// add-ons that the Makefile and the release scripts read through hack/addons.sh: ci.yml's add-on matrix, security.yml's
// govulncheck module and trivy image of each add-on, the cache prefixes that both workflows' cache-prune jobs prune,
// and release.yml, whose build, sign, verify and publish steps it runs for a list of its own, with fakes of cosign and
// gh: the build's addons output of each add-on's image, the images signed and verified, and the line of the release
// notes that hack/security/select-release-images.sh reads, written and checked in an existing release. An add-on
// listed but missing from a workflow would be released untested, unscanned or unsigned, and one in a workflow but not
// listed would be tested but never released, with nothing else failing. It also holds the chart's
// agentkourier.addon.names, the add-ons the chart renders, to the list, and hack/addons.sh to refusing every name the
// workflows already give to a job, a check, a cache, an image or a release output.
package addonlist
