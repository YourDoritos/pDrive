package daemon

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// WatchResume triggers a sync when the machine wakes from suspend.
//
// This is the case the whole freshness design exists for: you close the lid,
// something changes on another device, you open the lid. Polling on a timer
// would leave the folder stale for up to a full interval right at the moment
// someone is looking at it. logind announces the transition, so the sync is
// already running before the screen comes back.
func (d *Daemon) WatchResume(ctx context.Context) {
	conn, err := dbus.SystemBus()
	if err != nil {
		d.log.Debugf("no system bus; suspend/resume detection disabled: %v", err)
		return
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"),
	); err != nil {
		d.log.Debugf("could not subscribe to PrepareForSleep: %v", err)
		return
	}

	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	d.log.Infof("watching for resume from suspend")

	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			if len(sig.Body) != 1 {
				continue
			}
			going, _ := sig.Body[0].(bool)
			// true = about to sleep, false = just woke up.
			if !going {
				d.log.Infof("resumed from suspend; syncing")
				d.Trigger(TriggerResume)
			}
		}
	}
}
