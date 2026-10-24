// identical, and rooms' own saves are suppressed through savingFromRoom.

// Ports backend/apps/collab/signals.py
// Ports backend/apps/collab/notify.py
package doory

// notifyDesignTitle ports signals.design_saved for a title change: the room
// updates its title and broadcasts {"type":"title","title":...}.
func (a *App) notifyDesignTitle(designID, title string) {
	if !a.Settings.RealtimeEnabled {
		return
	}
	a.collabRegistry().notify(designID, "collab.title", map[string]any{"title": title})
}

// notifyDesignRecheck ports signals.design_saved (document/trashed_at),
// design_deleted and link_changed: the room re-checks access and reloads.
func (a *App) notifyDesignRecheck(designID string) {
	if !a.Settings.RealtimeEnabled {
		return
	}
	a.collabRegistry().notify(designID, "collab.recheck", nil)
}

// notifyDesignsRecheck fans the re-check out over a set of designs (bulk
// delete/trash sweeps).
func (a *App) notifyDesignsRecheck(ids []string) {
	for _, id := range ids {
		a.notifyDesignRecheck(id)
	}
}
