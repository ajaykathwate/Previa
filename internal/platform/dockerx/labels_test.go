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
