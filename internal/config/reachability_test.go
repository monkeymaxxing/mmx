package config

import (
	"mmx/internal/protocol"
	"strings"
	"testing"
)

func TestNodeJobValidatesReachabilityConnectionEligibility(t *testing.T) {
	for _, test := range []struct {
		kinds string
		valid bool
	}{
		{`[]`, true}, {`["ssh"]`, true}, {`["https","ssh"]`, true}, {`["ssh","ssh"]`, false}, {`["http"]`, false},
	} {
		var job Job
		err := protocol.Decode([]byte(`{"reachability_confirmation_connection_kinds":`+test.kinds+`}`), &job)
		if (err == nil) != test.valid {
			t.Fatal(test.kinds, err)
		}
		if err != nil && !strings.Contains(err.Error(), "reachability_confirmation_connection_kinds") {
			t.Fatal(err)
		}
	}
}
