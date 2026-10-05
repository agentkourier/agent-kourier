//go:build image

package testhooks_test

import (
	"debug/elf"
	"os"
	"strings"
	"testing"
)

// assertTraces fails for every trace of the test hooks in the binary at bin whose presence differs from want.
func assertTraces(t *testing.T, name, bin string, want bool) {
	t.Helper()
	for what, present := range imageTraces(t, bin) {
		switch {
		case present && !want:
			t.Errorf("%s: %s is in the binary", name, what)
		case !present && want:
			t.Errorf("%s: %s is missing, so the absence of it from the production binary proves nothing", name, what)
		}
	}
}

// TestBinaryHasNoHooks proves of one production binary, $COURIER_BINARY, what TestImageHasNeitherHook proves
// of the local image: it carries neither test hook. $COURIER_HOOKED_BINARY is the control, the same
// source and platform built with the testhooks tag, which must show every trace or the check is vacuous.
// The Dockerfile's verify stage runs this on each platform's binary before the image is assembled, so a
// release image of any architecture cannot exist with a hook in it. Both variables are required: a
// run without them is a wiring mistake, not a pass.
func TestBinaryHasNoHooks(t *testing.T) {
	prod, hooked := os.Getenv("COURIER_BINARY"), os.Getenv("COURIER_HOOKED_BINARY")
	if prod == "" || hooked == "" {
		t.Fatal("set COURIER_BINARY (the production binary) and COURIER_HOOKED_BINARY (the testhooks build of it)")
	}
	assertTraces(t, "COURIER_BINARY", prod, false)
	assertTraces(t, "COURIER_HOOKED_BINARY", hooked, true)
}

// TestBinaryIdentity proves of the same binary that it is a linux ELF for $COURIER_GOARCH (amd64 or arm64)
// and that main.version in it is $COURIER_VERSION. It reads the symbol table and the string the linker's -X
// wrote, so it works on a binary of any architecture, which the build host cannot run. The Dockerfile's
// verify stage runs it with the build's own platform and version.
func TestBinaryIdentity(t *testing.T) {
	bin, version, arch := os.Getenv("COURIER_BINARY"), os.Getenv("COURIER_VERSION"), os.Getenv("COURIER_GOARCH")
	if bin == "" || version == "" || arch == "" {
		t.Fatal("set COURIER_BINARY, COURIER_VERSION and COURIER_GOARCH")
	}
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if f.Machine != machine {
		t.Errorf("machine = %v, want %v (GOARCH=%s)", f.Machine, machine, arch)
	}
	if got := versionString(t, f); got != version {
		t.Errorf("main.version = %q, want %q", got, version)
	}
}

// versionString reads the bytes of the symbol main.version.str, where the linker puts the value of -X main.version, without its terminating NUL.
func versionString(t *testing.T, f *elf.File) string {
	t.Helper()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("symbols (a stripped binary has none): %v", err)
	}
	for _, sym := range syms {
		if sym.Name != "main.version.str" {
			continue
		}
		for _, sec := range f.Sections {
			if sec.Type == elf.SHT_NOBITS || sym.Value < sec.Addr || sym.Value+sym.Size > sec.Addr+sec.Size {
				continue
			}
			data, err := sec.Data()
			if err != nil {
				t.Fatal(err)
			}
			off := sym.Value - sec.Addr
			// The linker NUL-terminates the string and counts the NUL in the symbol.
			return strings.TrimSuffix(string(data[off:off+sym.Size]), "\x00")
		}
		t.Fatalf("no section holds main.version.str at %#x", sym.Value)
	}
	t.Fatal("main.version.str is not in the binary: no -X main.version was applied")
	return ""
}
