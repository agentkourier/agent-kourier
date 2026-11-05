package k8ssecrets

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// keySet is the keys of a Secret that an informer keeps: the ones it names, or every one. It is never changed once made.
type keySet struct {
	all  bool
	keys map[string]struct{}
}

// allKeys is every key of a Secret.
func allKeys() keySet { return keySet{all: true} }

// keysOf is the keys given.
func keysOf(keys ...string) keySet {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return keySet{keys: m}
}

// has reports whether s keeps the key.
func (s keySet) has(key string) bool {
	_, ok := s.keys[key]
	return s.all || ok
}

// covers reports whether s keeps every key that o does.
func (s keySet) covers(o keySet) bool {
	if s.all {
		return true
	}
	if o.all {
		return false
	}
	for k := range o.keys {
		if _, ok := s.keys[k]; !ok {
			return false
		}
	}
	return true
}

// union returns the keys that s or o keeps.
func (s keySet) union(o keySet) keySet {
	switch {
	case s.all || o.all:
		return allKeys()
	case s.covers(o):
		return s
	case o.covers(s):
		return o
	}
	m := maps.Clone(s.keys)
	maps.Copy(m, o.keys)
	return keySet{keys: m}
}

// equal reports whether s and o keep the same keys.
func (s keySet) equal(o keySet) bool { return s.covers(o) && o.covers(s) }

// trimTo is the informer transform of an informer that keeps the keys of keep. It reduces a Secret to its identity, its type and the
// keys of keep that it has, with StringData folded in as the API server does on write (it wins over Data for a key in both). The
// other keys are not kept, nor are their names: a Secret may hold a megabyte of them, and the writers of the namespace choose which.
// Only an Opaque Secret keeps its values; any other type keeps the names of the keys, with nil values. It returns anything that is
// not a Secret unchanged.
//
// It must never return an error: when the store rejects an object, client-go's reflector
// logs the untransformed object.
func trimTo(keep keySet) cache.TransformFunc {
	return func(obj any) (any, error) {
		s, ok := obj.(*corev1.Secret)
		if !ok {
			return obj, nil
		}
		size := len(keep.keys)
		if keep.all {
			size = len(s.Data) + len(s.StringData)
		}
		out := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion},
			Type:       s.Type,
			Data:       make(map[string][]byte, size),
		}
		for k, v := range s.Data {
			if keep.has(k) {
				out.Data[k] = v
			}
		}
		for k, v := range s.StringData {
			if keep.has(k) {
				out.Data[k] = []byte(v)
			}
		}
		if !opaque(s.Type) {
			for k := range out.Data {
				out.Data[k] = nil
			}
		}
		return out, nil
	}
}
