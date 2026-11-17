package v1alpha1

import (
	"fmt"
	"slices"
	"testing"
)

// The enum of spec.web.profile is webProfiles, in its order, which Validate names when it refuses a profile, and each has rules in the
// platform table (ChatPlatformOf).
func TestWebProfilesFollowTheEnum(t *testing.T) {
	enum, _ := crdSchema(t, "chatconnections").at(t, "spec.web.profile")["enum"].([]any)
	var names, profiles []string
	for _, v := range enum {
		names = append(names, fmt.Sprint(v))
	}
	for _, p := range webProfiles() {
		profiles = append(profiles, string(p))
		platformOf(t, ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: p}})
	}
	if !slices.Equal(names, profiles) {
		t.Errorf("the enum of spec.web.profile is %v, and webProfiles %v", names, profiles)
	}
}
