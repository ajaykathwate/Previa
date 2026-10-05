package dockerx

import "testing"

func TestOwnedRequiresManagedLabelAndKind(t *testing.T) {
	preview := map[string]string{LabelManaged: "true", LabelKind: KindPreview}
	if !Owned(preview, KindPreview) || !Owned(preview, KindBuilder, KindPreview) {
		t.Fatal("a managed preview must be owned")
	}
	for name, labels := range map[string]map[string]string{
		"other kind":   {LabelManaged: "true", LabelKind: KindBuilder},
		"not managed":  {LabelManaged: "false", LabelKind: KindPreview},
		"no labels":    nil,
		"forged kind":  {LabelKind: KindPreview},
		"postgres-ish": {"com.docker.compose.service": "postgres"},
	} {
		if Owned(labels, KindPreview) {
			t.Errorf("%s: treated as an owned preview", name)
		}
	}
}

func TestOwnedByRequiresThisInstance(t *testing.T) {
	const instance = "0a1b2c3d4e5f"
	mine := map[string]string{LabelManaged: "true", LabelKind: KindPreview, LabelInstance: instance}
	if !OwnedBy(mine, instance, KindPreview) {
		t.Fatal("this instance's preview must be owned")
	}
	for name, labels := range map[string]map[string]string{
		"another instance": {LabelManaged: "true", LabelKind: KindPreview, LabelInstance: "ffffffffffff"},
		"no instance":      {LabelManaged: "true", LabelKind: KindPreview},
		"other kind":       {LabelManaged: "true", LabelKind: KindBuilder, LabelInstance: instance},
	} {
		if OwnedBy(labels, instance, KindPreview) {
			t.Errorf("%s: treated as this instance's preview", name)
		}
	}
	if OwnedBy(map[string]string{LabelManaged: "true", LabelKind: KindPreview, LabelInstance: ""}, "", KindPreview) {
		t.Fatal("an empty instance owns nothing")
	}
	for _, bad := range []string{"", "short", "UPPERCASE1", "has-dash-1", "0123456789abcdef0123456789abcdef0"} {
		if ValidInstance(bad) {
			t.Errorf("%q accepted as an instance", bad)
		}
	}
}
