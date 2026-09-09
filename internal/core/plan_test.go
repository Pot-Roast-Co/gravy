package core

import "testing"

func TestValidatePlan(t *testing.T) {
	ok := func(title string, deps ...int) PlannedTicket {
		return PlannedTicket{Title: title, Route: RouteImplementation, DependsOn: deps}
	}

	for _, tc := range []struct {
		name    string
		tickets []PlannedTicket
		wantErr string
	}{
		{
			name:    "a single ticket with no dependencies",
			tickets: []PlannedTicket{ok("add a thing")},
		},
		{
			name:    "a linked set",
			tickets: []PlannedTicket{ok("schema"), ok("api", 0), ok("ui", 1)},
		},
		{
			name:    "two tickets depending on the same one",
			tickets: []PlannedTicket{ok("schema"), ok("api", 0), ok("cli", 0)},
		},
		{
			name:    "no tickets at all",
			tickets: nil,
			wantErr: "the plan has no tickets",
		},
		{
			name:    "a ticket with no title",
			tickets: []PlannedTicket{{Route: RouteImplementation}},
			wantErr: "ticket 1 has no title",
		},
		{
			name:    "a dependency index off the end",
			tickets: []PlannedTicket{ok("api", 5)},
			wantErr: "ticket 1 depends on 6, which is not in the plan",
		},
		{
			name:    "a negative dependency index",
			tickets: []PlannedTicket{ok("api", -1)},
			wantErr: "not in the plan",
		},
		{
			name:    "a ticket depending on itself",
			tickets: []PlannedTicket{ok("api", 0)},
			wantErr: "ticket 1 depends on itself",
		},
		{
			name:    "the same dependency twice",
			tickets: []PlannedTicket{ok("schema"), ok("api", 0, 0)},
			wantErr: "ticket 2 depends on 1 twice",
		},
		// The case that would otherwise leave two tickets blocked in the backlog forever with
		// no error reported anywhere.
		{
			name:    "a two-ticket cycle",
			tickets: []PlannedTicket{ok("a", 1), ok("b", 0)},
			wantErr: "dependency cycle",
		},
		{
			name:    "a longer cycle",
			tickets: []PlannedTicket{ok("a", 2), ok("b", 0), ok("c", 1)},
			wantErr: "dependency cycle",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePlan(tc.tickets)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidatePlan() = %v, want nil", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("ValidatePlan() = nil, want %q", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidatePlan() = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
