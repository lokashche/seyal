// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"slices"

	"seyal/internal/engine"
)

// CanDecide returns the approval slots the person may decide now: waiting, one of their roles, and
// they haven't decided another slot of this version (one decision per person per version). It is nil
// when there are none. st is the status of the item's version the person would decide on.
//
// My Work and the Work Item page ask it for the person looking (D85: it moved here from the web
// server), and the phone-note reader (internal/notify) asks it for everyone in the business, outside
// any web request.
func CanDecide(st engine.ApprovalStatus, p engine.Person) map[string]bool {
	for _, slot := range st.Slots {
		if slot.ApprovedBy == p.ID {
			return nil
		}
	}
	var ok map[string]bool
	for _, slot := range st.Slots {
		if slices.Contains(st.Waiting, slot.Slot) && slices.Contains(p.Roles, slot.Role) {
			if ok == nil {
				ok = map[string]bool{}
			}
			ok[slot.Slot] = true
		}
	}
	return ok
}
