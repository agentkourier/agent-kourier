package config

import "slices"

// ReferenceAllowed reports whether an object in namespace from may reference an object in namespace target whose
// spec.allowedNamespaces is allowed: a reference within one namespace always, a reference across namespaces only when the
// target lists from.
func ReferenceAllowed(from, target string, allowed []string) bool {
	return from == target || slices.Contains(allowed, from)
}
