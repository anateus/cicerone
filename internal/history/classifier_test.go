package history

import (
	"cicerone/internal/domain"
	"testing"
)

func TestClassifyPriorityAndLifecycle(t *testing.T) {
	base := Definition{Name: "foo", FullName: "foo", Type: domain.PackageFormula, Version: "1", Revision: "0", Homepage: "a"}
	tests := []struct {
		name          string
		before, after *Definition
		kind          domain.EventKind
		ambiguous     bool
	}{
		{"addition", nil, &base, domain.EventVersion, false}, {"deletion", &base, nil, domain.EventMetadata, false},
		{"version wins", &base, ptrDef(with(base, "2", "1", "b")), domain.EventVersion, false},
		{"revision wins", &base, ptrDef(with(base, "1", "1", "b")), domain.EventRevision, false},
		{"metadata", &base, ptrDef(with(base, "1", "0", "b")), domain.EventMetadata, false},
		{"missing identity", &Definition{Version: "1"}, &Definition{Version: "2"}, "", true},
		{"missing version before edit", &Definition{Name: "foo", Type: domain.PackageFormula}, &Definition{Name: "foo", Type: domain.PackageFormula, Homepage: "new"}, "", true},
		{"missing version deletion", &Definition{Name: "foo", Type: domain.PackageFormula}, nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.before, tc.after)
			if got.Kind != tc.kind || got.Ambiguous != tc.ambiguous {
				t.Fatalf("classification=%#v", got)
			}
		})
	}
}

func with(d Definition, version, revision, homepage string) Definition {
	d.Version = version
	d.Revision = revision
	d.Homepage = homepage
	return d
}
func ptrDef(d Definition) *Definition { return &d }
