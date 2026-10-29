package webconsole

import "testing"

func TestPublicIPv4Validation(t *testing.T) {
	if address, ok := publicIPv4("8.8.8.8"); !ok || address.String() != "8.8.8.8" {
		t.Fatalf("publicIPv4() = %v, %v", address, ok)
	}
	for _, value := range []string{"10.0.0.1", "2001:4860:4860::8888", "not-an-ip"} {
		if _, ok := publicIPv4(value); ok {
			t.Fatalf("publicIPv4(%q) accepted", value)
		}
	}
}

func TestParseBulkIPRequestRejectsDuplicateTargets(t *testing.T) {
	request := bulkIPRequest{PanelID: "main"}
	request.Mappings = append(request.Mappings,
		struct {
			OldIP string `json:"oldIp"`
			NewIP string `json:"newIp"`
		}{OldIP: "8.8.8.8", NewIP: "1.1.1.1"},
		struct {
			OldIP string `json:"oldIp"`
			NewIP string `json:"newIp"`
		}{OldIP: "9.9.9.9", NewIP: "1.1.1.1"},
	)
	if _, ok := parseBulkIPRequest(request); ok {
		t.Fatal("duplicate target IP accepted")
	}
}

func TestSplitNodeOperation(t *testing.T) {
	panel, node, ok := splitNodeOperation("/api/nodes/main/node-1/move", "move")
	if !ok || panel != "main" || node != "node-1" {
		t.Fatalf("splitNodeOperation() = %q, %q, %v", panel, node, ok)
	}
	if _, _, ok := splitNodeOperation("/api/nodes/main/node-1/ticket", "move"); ok {
		t.Fatal("unexpected operation match")
	}
}
