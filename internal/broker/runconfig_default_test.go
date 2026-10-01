//go:build !testhooks

package broker

import (
	"reflect"
	"testing"
)

// The production build has no way to start without a renderer or without interactions: the fields
// that allow it are not in RunConfig at all, so no code, and no config, can set them. (35h deletes
// them from the testhooks build too, once there is a renderer to run with.)
func TestTheDefaultBuildHasNoNoRendererOrNoInteractions(t *testing.T) {
	typ := reflect.TypeOf(RunConfig{})
	for _, name := range []string{"NoRenderer", "NoInteractions"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("RunConfig has a %s field in the default build", name)
		}
	}
}
