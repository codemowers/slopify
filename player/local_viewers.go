package main

type localViewer struct {
	evicted chan struct{}
	stop    func()
}

// Retain the newest three viewers. Closing an old stream is accompanied by an
// eviction event so EventSource does not reconnect and evict a newer viewer.
func (l *localStatus) addViewer(viewer *localViewer) {
	l.eventsMu.Lock()
	var oldest *localViewer
	if len(l.viewers) == 3 {
		oldest = l.viewers[0]
		l.viewers = l.viewers[1:]
	}
	l.viewers = append(l.viewers, viewer)
	l.eventsMu.Unlock()
	if oldest != nil {
		oldest.stop()
	}
}

func (l *localStatus) removeViewer(viewer *localViewer) {
	l.eventsMu.Lock()
	defer l.eventsMu.Unlock()
	for i, v := range l.viewers {
		if v == viewer {
			l.viewers = append(l.viewers[:i], l.viewers[i+1:]...)
			return
		}
	}
}
