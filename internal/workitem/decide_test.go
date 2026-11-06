// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"fmt"
	"testing"

	"seyal/internal/engine"
)

func TestCanDecide(t *testing.T) {
	st := engine.ApprovalStatus{
		Slots: []engine.SlotStatus{
			{Slot: "ops", Role: "ops", Required: true},
			{Slot: "sales", Role: "sales", Required: true, ApprovedBy: "p-asha"},
			{Slot: "owner", Role: "owner", Required: true},
			{Slot: "extra", Role: "owner"}, // not required now
		},
		Waiting: []string{"ops", "owner"},
	}
	tests := []struct {
		name string
		p    engine.Person
		want string
	}{
		{"one role", engine.Person{ID: "p-ben", Roles: []string{"ops"}}, "map[ops:true]"},
		{"two roles", engine.Person{ID: "p-chitra", Roles: []string{"ops", "owner", "admin"}}, "map[ops:true owner:true]"},
		{"already decided another slot of this version", engine.Person{ID: "p-asha", Roles: []string{"sales", "owner"}}, "map[]"},
		{"no role that waits", engine.Person{ID: "p-rahul", Roles: []string{"sales"}}, "map[]"},
		{"no roles", engine.Person{ID: "p-guest"}, "map[]"},
	}
	for _, tt := range tests {
		if got := fmt.Sprint(CanDecide(st, tt.p)); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
	if CanDecide(engine.ApprovalStatus{}, engine.Person{ID: "p-ben", Roles: []string{"ops"}}) != nil {
		t.Error("nothing waits, yet Ben may decide")
	}
}
