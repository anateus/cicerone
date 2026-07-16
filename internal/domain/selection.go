package domain

// Anchor identifies a selected feed item independently of its row position.
type Anchor struct {
	GroupID        EventID
	ChildEventID   EventID
	ViewportOffset int
	FallbackIndex  int
}

// RestoreSelection restores an anchor after the feed's contents change.
func RestoreSelection(old Anchor, groups []FeedGroup) Anchor {
	if len(groups) == 0 {
		return Anchor{}
	}

	if old.ChildEventID != "" {
		for index, group := range groups {
			for _, event := range group.Events {
				if event.ID == old.ChildEventID {
					return Anchor{
						GroupID:        group.ID,
						ChildEventID:   old.ChildEventID,
						ViewportOffset: old.ViewportOffset,
						FallbackIndex:  index,
					}
				}
			}
		}
	}

	for index, group := range groups {
		if group.ID == old.GroupID {
			return Anchor{
				GroupID:        group.ID,
				ViewportOffset: old.ViewportOffset,
				FallbackIndex:  index,
			}
		}
	}

	index := old.FallbackIndex
	if index < 0 {
		index = 0
	}
	if index >= len(groups) {
		index = len(groups) - 1
	}
	return Anchor{GroupID: groups[index].ID, FallbackIndex: index}
}
