package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTriageConversation(t *testing.T) {
	c := TriageConversation("check the fleet")
	if len(c.Messages) != 1 || c.Messages[0].Role != RoleUser || c.Messages[0].Content != "check the fleet" {
		t.Fatalf("messages %+v", c.Messages)
	}
	if !IsTriage(c) || IsTriage(Opening(referenceIntent, loadWorld(t), loadPacks(t))) {
		t.Fatal("IsTriage does not tell the triage call from a plan attempt")
	}
	var schema map[string]any
	if err := json.Unmarshal(c.Schema, &schema); err != nil {
		t.Fatalf("the triage schema is not JSON: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("the triage schema must stay closed")
	}
	// The model writes the properties in the schema's order: what is asked
	// comes before the verdict.
	if r, m := strings.Index(string(c.Schema), `"reason":{`), strings.Index(string(c.Schema), `"mission":{`); r < 0 || m < 0 || r > m {
		t.Fatal("reason must come before mission in the triage schema")
	}
}

func TestReadTriage(t *testing.T) {
	cases := map[string]struct {
		reply      string
		mission    bool
		reason     string
		unreadable bool
	}{
		"mission":          {`{"reason":"Sweep the east area.","mission":true}`, true, "Sweep the east area.", false},
		"declined":         {`{"reason":" A status request. ","mission":false}`, false, "A status request.", false},
		"trailing data":    {`{"reason":"r","mission":false} {}`, true, "", true},
		"mission as text":  {`{"reason":"r","mission":"no"}`, true, "", true},
		"null reason":      {`{"reason":null,"mission":false}`, true, "", true},
		"empty":            {``, true, "", true},
		"mission, no text": {`{"reason":"","mission":true}`, true, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := readTriage(tc.reply)
			if got.Reply != tc.reply || got.Mission != tc.mission || got.Reason != tc.reason || (got.Unreadable != "") != tc.unreadable {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
