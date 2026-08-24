package version

import "testing"

func TestString(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = old })

	if got, want := String(), "abbs 1.2.3"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name          string
		linkerVersion string
		moduleVersion string
		want          string
	}{
		{name: "linker version wins", linkerVersion: "2.0.0", moduleVersion: "v1.9.0", want: "2.0.0"},
		{name: "linker v is normalized", linkerVersion: "v2.0.0", want: "2.0.0"},
		{name: "go install module version", linkerVersion: developmentVersion, moduleVersion: "v1.4.2", want: "1.4.2"},
		{name: "development build", linkerVersion: developmentVersion, moduleVersion: "(devel)", want: developmentVersion},
		{name: "empty metadata", want: developmentVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.linkerVersion, tt.moduleVersion); got != tt.want {
				t.Fatalf("resolve(%q, %q) = %q, want %q", tt.linkerVersion, tt.moduleVersion, got, tt.want)
			}
		})
	}
}
