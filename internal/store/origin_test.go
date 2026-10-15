package store

import (
	"strings"
	"testing"
)

func TestOriginValid(t *testing.T) {
	for name, origin := range map[string]Origin{
		"ordinary conversation":      {},
		"source without subject":     {Source: "ci"},
		"generic source and subject": {Source: "ci", Subject: "build"},
		"identifier punctuation":     {Source: "alertmanager", Subject: "KubePodCrashLooping:v2"},
	} {
		t.Run(name, func(t *testing.T) {
			if !origin.Valid() {
				t.Errorf("Origin(%+v).Valid() = false; want true", origin)
			}
		})
	}
	for name, origin := range map[string]Origin{
		"subject without source": {Subject: "build"},
		"source with space":      {Source: "build system"},
		"subject with newline":   {Source: "ci", Subject: "build\n1"},
		"source too long":        {Source: strings.Repeat("a", MaxOriginPart+1)},
		"subject too long":       {Source: "ci", Subject: strings.Repeat("a", MaxOriginPart+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if origin.Valid() {
				t.Errorf("Origin(%+v).Valid() = true; want false", origin)
			}
		})
	}
}
