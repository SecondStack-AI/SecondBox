package networkpolicycontract

import "testing"

func TestNormalizeLogicalGatewayName(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "gateway.example", want: "gateway.example"},
		{name: "GATEWAY.EXAMPLE.", want: "gateway.example"},
		{name: "gateway_name.example", wantErr: true},
		{name: "192.0.2.10", wantErr: true},
		{name: "gateway..example", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeLogicalGatewayName(test.name)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("NormalizeLogicalGatewayName(%q) = %q, %v", test.name, got, err)
			}
		})
	}
}
