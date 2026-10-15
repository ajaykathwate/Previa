package routing

import "testing"

func TestParseServiceRequestsKeepsOnlyPreviewServices(t *testing.T) {
	got := parseServiceRequests("preview-12-g1-r3@file 43\npreview-7-g2-r1@file 1200000\ndashboard@file 9\npreview-x@file 3\n\npreview-9-g1-r1@file -1\n")
	if len(got) != 2 || got["preview-12-g1-r3@file"] != 43 || got["preview-7-g2-r1@file"] != 1200000 {
		t.Fatalf("counts: %v", got)
	}
}
